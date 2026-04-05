// Package forward provides the WebSocket tunnel endpoint for port-forwarding.
//
// Routes:
//
//	GET /api/v1/forward/{project}/{service}/{port} — WebSocket upgrade + TCP tunnel
package forward

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

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
	logger    *zap.Logger
	inspector ContainerInspector
	upgrader  websocket.Upgrader
}

// NewHandler creates a Handler.
func NewHandler(logger *zap.Logger, inspector ContainerInspector) *Handler {
	return &Handler{
		logger:    logger,
		inspector: inspector,
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
		http.Error(w, fmt.Sprintf("invalid port: %s", portStr), http.StatusBadRequest)
		return
	}

	info, err := h.inspector.InspectContainer(r.Context(), project, service)
	if err != nil {
		log.Warn("Container inspect failed", zap.Error(err))
		http.Error(w, fmt.Sprintf("container %s-%s not found", project, service), http.StatusNotFound)
		return
	}

	if !info.Running {
		http.Error(w, fmt.Sprintf("container %s-%s is not running", project, service), http.StatusConflict)
		return
	}

	// Dial the container's port on the bridge network before upgrading so we
	// can return a proper HTTP error if the port is unreachable.
	target := net.JoinHostPort(info.IP, portStr)
	tcpConn, err := net.Dial("tcp", target)
	if err != nil {
		log.Warn("Failed to dial container port", zap.String("target", target), zap.Error(err))
		http.Error(w, fmt.Sprintf("cannot reach %s: %v", target, err), http.StatusBadGateway)
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
