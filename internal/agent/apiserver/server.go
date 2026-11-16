// Package apiserver provides the Agent-side HTTP server.
//
// It mirrors the control-plane apiserver pattern (RouteRegistrar, mux-based
// routing) but is intentionally kept minimal — the Agent only needs a health
// probe, the logs and port-forward endpoints, and a probe for cara-server.
//
// Everything but GET /healthz accepts only requests cara-server signed
// (CARA-139); see package agentauth.
package apiserver

import (
	"net/http"

	"NYCU-SDC/caravanserai/internal/agentauth"

	"go.uber.org/zap"
)

// ProbePath is what cara-server's node probe calls. Unlike /healthz it needs
// a signature, so a probe that succeeds shows the Agent accepts the server's.
const ProbePath = "/api/v1/probe"

// RouteRegistrar is implemented by any handler package that wants to mount its
// own routes onto the Agent's HTTP mux.
type RouteRegistrar interface {
	RegisterRoutes(mux *http.ServeMux)
}

// Server holds the Agent HTTP mux and logger.
type Server struct {
	logger   *zap.Logger
	mux      *http.ServeMux
	verifier *agentauth.Verifier
}

// New creates a Server with the built-in probes already mounted. Requests
// other than GET /healthz must carry an assertion verifier accepts.
func New(logger *zap.Logger, verifier *agentauth.Verifier) *Server {
	if verifier == nil {
		panic("agent apiserver.New: verifier is required")
	}
	s := &Server{
		logger:   logger,
		mux:      http.NewServeMux(),
		verifier: verifier,
	}
	s.registerBuiltinRoutes()
	return s
}

// registerBuiltinRoutes mounts the probes.
func (s *Server) registerBuiltinRoutes() {
	ok := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			s.logger.Error("Failed to write probe response", zap.String("path", r.URL.Path), zap.Error(err))
		}
	}
	s.mux.HandleFunc("GET "+agentauth.HealthPath, ok)
	s.mux.HandleFunc("GET "+ProbePath, ok)
}

// Register mounts a RouteRegistrar's routes onto the mux.
func (s *Server) Register(registrar RouteRegistrar) {
	registrar.RegisterRoutes(s.mux)
}

// Handler returns the final http.Handler to pass to http.Server.
func (s *Server) Handler() http.Handler {
	return agentauth.Middleware(s.verifier, s.logger, s.mux)
}
