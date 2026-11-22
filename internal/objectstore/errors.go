package objectstore

import "errors"

// ErrNotFound is returned by Get and Head when the requested key does not
// exist. Callers should check with errors.Is, not compare provider errors
// directly, since each backend reports "missing" differently.
var ErrNotFound = errors.New("objectstore: key not found")

// ErrPreconditionFailed is returned by a conditional Put (PutOptions.IfMatch
// or IfAbsent) whose condition did not hold: the object changed, appeared or
// disappeared since the caller looked. Nothing was written. Another writer
// got there first, so the caller must read the current state again rather
// than retry the same write.
var ErrPreconditionFailed = errors.New("objectstore: precondition failed")

// ErrInvalidOptions is returned by Put for options that cannot be honoured,
// such as a condition on an upload of unknown size.
var ErrInvalidOptions = errors.New("objectstore: invalid put options")
