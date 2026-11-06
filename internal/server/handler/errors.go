// Package handler provides shared utilities for HTTP handlers.
package handler

import (
	"context"
	"errors"
	"fmt"

	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/server/authz"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/problem"
)

// ErrServiceNotConfigured indicates a feature was invoked but the configuration
// it depends on is absent. It is a deployment/config gap, not a server fault, so
// it maps to 503 Service Unavailable rather than the default 500. Wrap it with
// %w to add a caller-specific detail.
var ErrServiceNotConfigured = errors.New("service not configured")

// ErrOperatorUnauthorized is returned when an admin-only route is reached with
// no authenticated caller. The authentication middleware normally answers
// first; this covers a handler served without it.
var ErrOperatorUnauthorized = errors.New("operator credential required")

// ErrForbidden is returned when the authenticated caller may not make the
// request. It is authz.ErrForbidden, so a refusal from either package maps to
// 403.
var ErrForbidden = authz.ErrForbidden

// ErrInvalidJoinToken is returned when a Node join presents no join token, or
// one that is unknown, used, revoked or expired.
var ErrInvalidJoinToken = errors.New("a valid, unused join token is required")

// RequireAdmin returns nil when the request's caller is an admin, and the error
// to write otherwise.
func RequireAdmin(ctx context.Context) error {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return ErrOperatorUnauthorized
	}
	if !p.IsAdmin() {
		return fmt.Errorf("%w: %s may not do this", ErrForbidden, p)
	}
	return nil
}

// NewProblemMapping returns a mapping function that bridges Caravanserai's
// store sentinel errors to summer's problem types. Pass the returned function
// to problem.NewWithMapping to create an HttpWriter.
//
// Mapped errors:
//   - store.ErrNotFound          → 404 Not Found
//   - store.ErrAlreadyExists     → 409 Conflict
//   - store.ErrConflictState     → 409 Conflict
//   - store.ErrVersionConflict   → 409 Conflict
//   - ErrServiceNotConfigured    → 503 Service Unavailable
//   - ErrOperatorUnauthorized    → 401 Unauthorized
//   - ErrForbidden               → 403 Forbidden
//   - store.ErrWrongNode         → 403 Forbidden
//   - ErrInvalidJoinToken        → 401 Unauthorized
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

		case errors.Is(err, ErrOperatorUnauthorized), errors.Is(err, ErrInvalidJoinToken):
			return problem.Problem{
				Title:  "Unauthorized",
				Status: 401,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/401",
				Detail: err.Error(),
			}

		case errors.Is(err, ErrForbidden), errors.Is(err, store.ErrWrongNode):
			return problem.Problem{
				Title:  "Forbidden",
				Status: 403,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/403",
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

		// Same status as ErrConflictState but a different meaning: nothing is
		// wrong with the request, the caller just lost a race and can repeat it
		// from the current state.
		case errors.Is(err, store.ErrVersionConflict):
			return problem.Problem{
				Title:  "Conflict",
				Status: 409,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
				Detail: err.Error(),
			}

		// A stale assignment is a 409 like the others, but the detail is fixed
		// rather than derived from the error: the underlying mismatch names the
		// UID, nodeRef, and generation the caller failed to match, and echoing
		// those back would expose internal ownership identity. The Agent does
		// not need them — it must stop and re-poll ownership, not repair the
		// fence from the response.
		case errors.Is(err, store.ErrStaleAssignment):
			return problem.Problem{
				Title:  "Conflict",
				Status: 409,
				Type:   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/409",
				Detail: "assignment is stale; the Project is no longer owned by this assignment",
			}

		default:
			return problem.Problem{}
		}
	}
}
