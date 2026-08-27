// Package handler provides shared utilities for HTTP handlers.
package handler

import (
	"errors"

	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/problem"
)

// ErrServiceNotConfigured indicates a feature was invoked but the configuration
// it depends on is absent. It is a deployment/config gap, not a server fault, so
// it maps to 503 Service Unavailable rather than the default 500. Wrap it with
// %w to add a caller-specific detail.
var ErrServiceNotConfigured = errors.New("service not configured")

// NewProblemMapping returns a mapping function that bridges Caravanserai's
// store sentinel errors to summer's problem types. Pass the returned function
// to problem.NewWithMapping to create an HttpWriter.
//
// Mapped errors:
//   - store.ErrNotFound          → 404 Not Found
//   - store.ErrAlreadyExists     → 409 Conflict
//   - store.ErrConflictState     → 409 Conflict
//   - ErrServiceNotConfigured    → 503 Service Unavailable
//
// Unrecognised errors return an empty Problem{}, which lets summer's built-in
// fallback logic handle them (typically producing a 500 Internal Server Error).
func NewProblemMapping() func(error) problem.Problem {
	return func(err error) problem.Problem {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return problem.NewNotFoundProblem(err.Error())

		case errors.Is(err, ErrServiceNotConfigured):
			return problem.Problem{
				Title:  "Service Unavailable",
				Status: 503,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/503",
				Detail: err.Error(),
			}

		case errors.Is(err, store.ErrAlreadyExists):
			return problem.Problem{
				Title:  "Conflict",
				Status: 409,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
				Detail: err.Error(),
			}

		case errors.Is(err, store.ErrConflictState):
			return problem.Problem{
				Title:  "Conflict",
				Status: 409,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
				Detail: err.Error(),
			}

		default:
			return problem.Problem{}
		}
	}
}
