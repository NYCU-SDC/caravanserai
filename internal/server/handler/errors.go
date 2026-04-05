// Package handler provides shared utilities for HTTP handlers.
package handler

import (
	"errors"

	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/problem"
)

// NewProblemMapping returns a mapping function that bridges Caravanserai's
// store sentinel errors to summer's problem types. Pass the returned function
// to problem.NewWithMapping to create an HttpWriter.
//
// Mapped errors:
//   - store.ErrNotFound     → 404 Not Found
//   - store.ErrAlreadyExists → 409 Conflict
//
// Unrecognised errors return an empty Problem{}, which lets summer's built-in
// fallback logic handle them (typically producing a 500 Internal Server Error).
func NewProblemMapping() func(error) problem.Problem {
	return func(err error) problem.Problem {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return problem.NewNotFoundProblem(err.Error())

		case errors.Is(err, store.ErrAlreadyExists):
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
