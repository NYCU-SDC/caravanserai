// Package objectstore provides a minimal S3-compatible object storage
// primitive shared by every S3-backed feature in cara (Managed volume backup,
// and later control-plane DR).
//
// This package deliberately knows nothing about Projects, volumes, backup
// generations, or leader leases — it only moves bytes at named keys. Higher
// layers (internal/agent/backup, and later a control-plane package) build
// their own key layouts and workflows on top of it. Keeping this boundary
// narrow is what lets both features share one client and one integration
// test suite while using different S3 prefixes, credentials, and failure
// handling.
package objectstore

import (
	"context"
	"io"
	"time"
)

// ObjectMeta describes a stored object without its body.
type ObjectMeta struct {
	// Key is the full object key.
	Key string
	// ETag is the provider-assigned entity tag, used to detect whether an
	// upload was received intact.
	ETag string
	// Size is the object size in bytes.
	Size int64
	// LastModified is when the object was last written.
	LastModified time.Time
}

// PutOptions configures an upload.
type PutOptions struct {
	// ContentType is the MIME type stored with the object. Callers that don't
	// care can leave it empty; the provider will default it.
	ContentType string
	// Size is the exact body length in bytes. Passing it when known lets the
	// client avoid buffering the whole body to compute it, which matters for
	// multi-gigabyte volume archives. Pass -1 if the size is not known ahead
	// of time — the zero value means a genuinely empty object, not "unknown",
	// so callers must set this field explicitly rather than relying on a
	// struct literal's default.
	Size int64
}

// ObjectStore is the narrow contract every S3-backed feature builds on.
// Implementations must be safe for concurrent use.
type ObjectStore interface {
	// Put uploads body to key, returning the metadata the provider assigned.
	// It does not create parent "directories" — S3-compatible stores are
	// flat; keys merely contain slashes.
	Put(ctx context.Context, key string, body io.Reader, opts PutOptions) (ObjectMeta, error)

	// Get downloads the object at key. The caller must close the returned
	// reader. Returns an error satisfying errors.Is(err, ErrNotFound) if the
	// key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectMeta, error)

	// Head fetches metadata for key without downloading its body. Returns an
	// error satisfying errors.Is(err, ErrNotFound) if the key does not exist.
	Head(ctx context.Context, key string) (ObjectMeta, error)

	// List returns metadata for every object whose key starts with prefix.
	// Callers that need "the latest backup" must not rely on this for
	// ordering or recency — see the manifest-based design in CARA-59 for why.
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)

	// Delete removes the object at key. It does not error if the key does
	// not exist, matching S3 semantics.
	Delete(ctx context.Context, key string) error
}
