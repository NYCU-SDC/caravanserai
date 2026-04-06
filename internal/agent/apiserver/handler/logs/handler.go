// Package logs provides the chunked HTTP log streaming endpoint.
//
// Routes:
//
//	GET /api/v1/logs/{project}/{service} — stream container logs
package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/docker/docker/pkg/stdcopy"
	"go.uber.org/zap"
)

// Sentinel errors for container-level problems.  These are mapped to HTTP
// status codes by NewProblemMapping.
var (
	// ErrContainerNotFound indicates the requested container does not exist.
	ErrContainerNotFound = errors.New("container not found")
	// ErrContainerNotRunning indicates the container exists but is stopped
	// (only relevant when follow=true).
	ErrContainerNotRunning = errors.New("container not running")
)

// NewProblemMapping returns the error→Problem mapping for the logs handler.
// Pass the result to problem.NewWithMapping.
func NewProblemMapping() func(error) problem.Problem {
	return func(err error) problem.Problem {
		switch {
		case errors.Is(err, ErrContainerNotFound):
			return problem.NewNotFoundProblem(err.Error())

		case errors.Is(err, ErrContainerNotRunning):
			return problem.Problem{
				Title:  "Conflict",
				Status: http.StatusConflict,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
				Detail: err.Error(),
			}

		default:
			return problem.Problem{} // fall through to summer built-in
		}
	}
}

// ContainerLogger is the narrow interface the logs handler needs to stream
// container logs.
type ContainerLogger interface {
	// ContainerLogs returns a streaming reader of the container logs for the
	// given project and service.
	ContainerLogs(ctx context.Context, project, service string, follow bool, tail string, timestamps bool) (docker.ContainerLogResult, error)
}

// Handler serves the chunked HTTP container log streaming endpoint.
type Handler struct {
	logger        *zap.Logger
	runtime       ContainerLogger
	problemWriter *problem.HttpWriter
}

// NewHandler creates a Handler.
func NewHandler(logger *zap.Logger, runtime ContainerLogger, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		runtime:       runtime,
		problemWriter: pw,
	}
}

// RegisterRoutes mounts the logs endpoint onto the mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/logs/{project}/{service}", h.streamLogs)
}

func (h *Handler) streamLogs(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	service := r.PathValue("service")

	log := h.logger.With(
		zap.String("project", project),
		zap.String("service", service),
	)

	// Parse query parameters.
	follow := parseBool(r.URL.Query().Get("follow"), false)
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "all"
	}
	timestamps := parseBool(r.URL.Query().Get("timestamps"), false)

	result, err := h.runtime.ContainerLogs(r.Context(), project, service, follow, tail, timestamps)
	if err != nil {
		log.Warn("Container logs failed", zap.Error(err))

		// Determine the sentinel error to use.
		sentinel := classifyError(err)
		h.problemWriter.WriteError(r.Context(), w,
			fmt.Errorf("container %s/%s: %w", project, service, sentinel), log)
		return
	}
	defer func() { _ = result.Reader.Close() }()

	// Set headers for chunked streaming.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error("ResponseWriter does not support Flusher")
		return
	}

	log.Info("Streaming container logs",
		zap.Bool("follow", follow),
		zap.String("tail", tail),
		zap.Bool("tty", result.TTY),
	)

	// Stream logs to the HTTP response.
	if result.TTY {
		// TTY containers produce a raw stream — copy directly.
		_, _ = io.Copy(newFlushWriter(w, flusher), result.Reader)
	} else {
		// Non-TTY containers produce a multiplexed stream with 8-byte
		// headers.  Demultiplex stdout+stderr into the response.
		_, _ = stdcopy.StdCopy(newFlushWriter(w, flusher), newFlushWriter(w, flusher), result.Reader)
	}

	log.Info("Log stream closed")
}

// classifyError maps a runtime error to the appropriate sentinel error.
func classifyError(err error) error {
	msg := err.Error()
	// The DockerRuntime returns "container X is not running" for follow on
	// stopped containers, and Docker's "not found" for missing containers.
	if containsAny(msg, "not found", "No such") {
		return ErrContainerNotFound
	}
	if containsAny(msg, "not running") {
		return ErrContainerNotRunning
	}
	return ErrContainerNotFound // default to not-found for unknown errors
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// parseBool parses a query parameter as a boolean, returning the default if
// the value is empty or unparseable.
func parseBool(s string, defaultVal bool) bool {
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return defaultVal
	}
	return v
}

// flushWriter wraps an http.ResponseWriter and flushes after every Write.
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func newFlushWriter(w io.Writer, f http.Flusher) *flushWriter {
	return &flushWriter{w: w, flusher: f}
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.flusher.Flush()
	return n, err
}

// Ensure DockerRuntime implements ContainerLogger at compile time.
var _ ContainerLogger = (*docker.DockerRuntime)(nil)
