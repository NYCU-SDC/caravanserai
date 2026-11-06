package cli

import "errors"

// Exit codes caractl returns. Anything not listed is 1.
const (
	// ExitTimeout: a wait ran out of time before its condition held.
	ExitTimeout = 2
	// ExitNotFound: a wait was about a resource that does not exist.
	ExitNotFound = 3
)

// exitError carries the exit code main should use for err.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withExitCode(code int, err error) error {
	return &exitError{code: code, err: err}
}

// ExitCode returns the process exit code for an error a command returned.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}
