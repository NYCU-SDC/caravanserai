package apiserver

import (
	"net/http"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"go.uber.org/zap"
)

// RouteRegistrar is implemented by any handler package that wants to mount its
// own routes onto the mux.  The convention mirrors the rest of the codebase:
// handlers own their routes, the apiserver just provides the mux and the
// middleware sets they need.
type RouteRegistrar interface {
	// RegisterRoutes mounts the handler's routes onto mux using the provided
	// middleware sets.  Implementations should use the named middleware sets
	// (e.g. basicMiddleware, authMiddleware) rather than depend on a specific
	// ordering of wrappers.
	RegisterRoutes(mux *http.ServeMux, basicMiddleware *middleware.Set)
}

// Server holds the HTTP mux and the shared middleware that every route shares.
// It is the single assembly point for all HTTP concerns: the middleware chain is
// built once here and handed to each RouteRegistrar.
type Server struct {
	logger *zap.Logger

	mux             *http.ServeMux
	basicMiddleware *middleware.Set
}

// New creates a Server and wires up the recover + trace middleware chain.
// The caller is expected to call RegisterRoutes and then pass Handler() to
// http.Server.
func New(logger *zap.Logger, basicMiddleware *middleware.Set) *Server {
	s := &Server{
		logger:          logger,
		mux:             http.NewServeMux(),
		basicMiddleware: basicMiddleware,
	}

	s.registerBuiltinRoutes()

	return s
}

// registerBuiltinRoutes mounts the health probe.  This is always present and
// does not belong to any feature handler.
func (s *Server) registerBuiltinRoutes() {
	s.mux.Handle("GET /api/healthz", s.basicMiddleware.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			s.logger.Error("Failed to write healthz response", zap.Error(err))
		}
	}))
}

// Register mounts a RouteRegistrar's routes onto the mux.
// Call this once per handler before starting the HTTP server.
func (s *Server) Register(registrar RouteRegistrar) {
	registrar.RegisterRoutes(s.mux, s.basicMiddleware)
}

// Handler returns the final http.Handler to pass to http.Server.
// Call this after all registrars have been registered.
func (s *Server) Handler() http.Handler {
	return s.mux
}
