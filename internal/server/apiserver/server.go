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

	// authenticate wraps the whole mux, so no route can be registered outside
	// it. Nil serves without authentication, which only tests do.
	authenticate func(http.Handler) http.Handler
	// authorize wraps the mux inside authentication, so it sees the
	// principal. Nil serves without authorization, which only tests do.
	authorize func(*http.ServeMux) http.Handler
}

// Authenticator attaches the caller's identity to a request, or rejects it.
type Authenticator interface {
	Middleware(next http.Handler) http.Handler
}

// RequireAuthentication puts every route behind a. cara-server always calls
// it (ADR-0031).
func (s *Server) RequireAuthentication(a Authenticator) {
	s.authenticate = a.Middleware
}

// Authorizer decides what an authenticated caller may do (CARA-140). It wraps
// the mux itself because it needs to know which route a request matches.
type Authorizer interface {
	Wrap(mux *http.ServeMux) http.Handler
}

// RequireAuthorization puts every route behind a, inside authentication.
// cara-server always calls it.
func (s *Server) RequireAuthorization(a Authorizer) {
	s.authorize = a.Wrap
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
	var h http.Handler = s.mux
	if s.authorize != nil {
		h = s.authorize(s.mux)
	}
	if s.authenticate != nil {
		h = s.authenticate(h)
	}
	return h
}
