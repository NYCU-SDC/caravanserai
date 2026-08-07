package objectstore

import "errors"

// ErrNotFound is returned by Get and Head when the requested key does not
// exist. Callers should check with errors.Is, not compare provider errors
// directly, since each backend reports "missing" differently.
var ErrNotFound = errors.New("objectstore: key not found")
