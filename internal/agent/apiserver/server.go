// Package apiserver provides the Agent-side HTTP server.
//
// It mirrors the control-plane apiserver pattern (RouteRegistrar, mux-based
// routing) but is intentionally kept minimal — the Agent only needs a health
// probe and the port-forward WebSocket endpoint.
package apiserver

import (
	"net/http"

	"go.uber.org/zap"
)

// RouteRegistrar is implemented by any handler package that wants to mount its
// own routes onto the Agent's HTTP mux.
type RouteRegistrar interface {
	RegisterRoutes(mux *http.ServeMux)
}

// Server holds the Agent HTTP mux and logger.
type Server struct {
	logger *zap.Logger
	mux    *http.ServeMux
}

// New creates a Server with the built-in health probe already mounted.
func New(logger *zap.Logger) *Server {
	s := &Server{
		logger: logger,
		mux:    http.NewServeMux(),
	}
	s.registerBuiltinRoutes()
	return s
}

// registerBuiltinRoutes mounts the health probe.
func (s *Server) registerBuiltinRoutes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			s.logger.Error("Failed to write healthz response", zap.Error(err))
		}
	})
}

// Register mounts a RouteRegistrar's routes onto the mux.
func (s *Server) Register(registrar RouteRegistrar) {
	registrar.RegisterRoutes(s.mux)
}

// Handler returns the final http.Handler to pass to http.Server.
func (s *Server) Handler() http.Handler {
	return s.mux
}
