package trace

import (
	"net/http"

	traceutil "github.com/NYCU-SDC/summer/pkg/trace"
	"go.uber.org/zap"
)

// Middleware wraps the summer trace/recover utilities into http.HandlerFunc
// middleware compatible with the project's middleware.Set pattern.
type Middleware struct {
	logger *zap.Logger
	debug  bool
}

func NewMiddleware(logger *zap.Logger, debug bool) *Middleware {
	return &Middleware{logger: logger, debug: debug}
}

func (m *Middleware) TraceMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return traceutil.TraceMiddleware(next, m.logger, m.debug)
}

func (m *Middleware) RecoverMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return traceutil.RecoverMiddleware(next, m.logger, m.debug)
}
