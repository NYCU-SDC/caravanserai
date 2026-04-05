// Package forward provides the WebSocket tunnel endpoint for port-forwarding.
//
// Routes:
//
//	GET /api/v1/forward/{project}/{service}/{port} — WebSocket upgrade + TCP tunnel
package forward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Sentinel errors for container-level problems. These are mapped to HTTP
// status codes by NewProblemMapping.
var (
	// ErrContainerNotFound indicates the requested container does not exist.
	ErrContainerNotFound = errors.New("container not found")
	// ErrContainerNotRunning indicates the container exists but is stopped.
	ErrContainerNotRunning = errors.New("container not running")
	// ErrPortUnreachable indicates the container is running but the target
	// port could not be dialled.
	ErrPortUnreachable = errors.New("port unreachable")
)

// NewProblemMapping returns the error→Problem mapping for the forward handler.
// Pass the result to problem.NewWithMapping.
func NewProblemMapping() func(error) problem.Problem {
	return func(err error) problem.Problem {
		switch {
		case errors.Is(err, ErrContainerNotFound):
			return problem.NewNotFoundProblem(err.Error())

		case errors.Is(err, ErrContainerNotRunning):
			return problem.Problem{
				Title:  "Service Unavailable",
				Status: http.StatusServiceUnavailable,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/503",
				Detail: err.Error(),
			}

		case errors.Is(err, ErrPortUnreachable):
			return problem.Problem{
				Title:  "Bad Gateway",
				Status: http.StatusBadGateway,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/502",
				Detail: err.Error(),
			}

		default:
			return problem.Problem{} // fall through to summer built-in (400/500)
		}
	}
}

// ContainerInfo holds the information needed to connect to a container's port.
type ContainerInfo struct {
	// IP is the container's IP on the project bridge network.
	IP string
	// Running indicates whether the container is currently running.
	Running bool
}

// ContainerInspector is the narrow interface the forward handler needs to
// locate a container and obtain its network address.
type ContainerInspector interface {
	// InspectContainer returns the container's network info for the given
	// project and service. Returns an error if the container does not exist.
	InspectContainer(ctx context.Context, project, service string) (ContainerInfo, error)
}

// Handler serves the WebSocket port-forward tunnel endpoint.
type Handler struct {
	logger        *zap.Logger
	inspector     ContainerInspector
	problemWriter *problem.HttpWriter
	upgrader      websocket.Upgrader
}

// NewHandler creates a Handler.
func NewHandler(logger *zap.Logger, inspector ContainerInspector, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		inspector:     inspector,
		problemWriter: pw,
		upgrader: websocket.Upgrader{
			// Allow all origins — the Agent is not a public-facing server.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// RegisterRoutes mounts the forward endpoint onto the mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/forward/{project}/{service}/{port}", h.forward)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	service := r.PathValue("service")
	portStr := r.PathValue("port")

	log := h.logger.With(
		zap.String("project", project),
		zap.String("service", service),
		zap.String("port", portStr),
	)

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		h.problemWriter.WriteError(r.Context(), w,
			handlerutil.NewValidationError("port", portStr, fmt.Sprintf("invalid port: %s", portStr)), log)
		return
	}

	info, err := h.inspector.InspectContainer(r.Context(), project, service)
	if err != nil {
		log.Warn("Container inspect failed", zap.Error(err))
		h.problemWriter.WriteError(r.Context(), w,
			fmt.Errorf("container %s-%s: %w", project, service, ErrContainerNotFound), log)
		return
	}

	if !info.Running {
		h.problemWriter.WriteError(r.Context(), w,
			fmt.Errorf("container %s-%s: %w", project, service, ErrContainerNotRunning), log)
		return
	}

	// Dial the container's port on the bridge network before upgrading so we
	// can return a proper HTTP error if the port is unreachable.
	target := net.JoinHostPort(info.IP, portStr)
	tcpConn, err := net.Dial("tcp", target)
	if err != nil {
		log.Warn("Failed to dial container port", zap.String("target", target), zap.Error(err))
		h.problemWriter.WriteError(r.Context(), w,
			fmt.Errorf("cannot reach %s: %v: %w", target, err, ErrPortUnreachable), log)
		return
	}

	// Upgrade to WebSocket.
	wsConn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("WebSocket upgrade failed", zap.Error(err))
		_ = tcpConn.Close()
		return // Upgrade already wrote the HTTP error response.
	}

	log.Info("Port-forward tunnel established", zap.String("target", target))

	// Bidirectional copy: WebSocket <-> TCP.
	done := make(chan struct{}, 2)

	// TCP -> WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := tcpConn.Read(buf)
			if n > 0 {
				if writeErr := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// WebSocket -> TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, readErr := wsConn.ReadMessage()
			if readErr != nil {
				return
			}
			if _, writeErr := tcpConn.Write(msg); writeErr != nil {
				return
			}
		}
	}()

	// Wait for either direction to finish.
	<-done

	_ = wsConn.Close()
	_ = tcpConn.Close()

	// Wait for the other goroutine to exit.
	<-done

	log.Info("Port-forward tunnel closed", zap.String("target", target))
}

// DockerInspector adapts docker.DockerRuntime to the ContainerInspector
// interface used by the forward handler.
type DockerInspector struct {
	runtime *docker.DockerRuntime
}

// NewDockerInspector creates a ContainerInspector backed by the Docker API.
func NewDockerInspector(runtime *docker.DockerRuntime) *DockerInspector {
	return &DockerInspector{runtime: runtime}
}

// InspectContainer implements ContainerInspector.
func (d *DockerInspector) InspectContainer(ctx context.Context, project, service string) (ContainerInfo, error) {
	containerName := docker.ContainerName(project, service)
	result, err := d.runtime.ContainerInspectRaw(ctx, containerName)
	if err != nil {
		return ContainerInfo{}, err
	}

	return ContainerInfo{
		IP:      result.NetworkIP,
		Running: result.Running,
	}, nil
}

// Ensure DockerInspector implements ContainerInspector at compile time.
var _ ContainerInspector = (*DockerInspector)(nil)
