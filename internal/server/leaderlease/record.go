// Package leaderlease keeps the control-plane leader lease in lease.json on an
// S3-compatible object store (CARA-71, ADR-0037).
//
// One cara-server holds the lease and renews it; standbys watch it and take
// it over when it stops changing. Every write is conditional on the state the
// writer last read, so among servers racing for the same term at most one
// wins.
//
// This file and store.go are the record and its storage. Deciding when to
// renew, take over or stop (the election) is built on top of Store and never
// reads RenewedAt: expiry is judged on each machine's own clock from whether
// the ETag changed (ADR-0037).
package leaderlease

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// CurrentVersion is the lease.json format this package writes. A lease with
// another version is refused rather than guessed at: a newer server may have
// written fields this one would drop on its next write.
const CurrentVersion = 1

// DefaultKey is where lease.json lives when the server's settings do not name
// a key.
const DefaultKey = "cara/v1/control-plane/lease.json"

// Record is the content of lease.json.
type Record struct {
	// Version is the format version; see CurrentVersion.
	Version int `json:"version"`
	// Term goes up by one each time a server takes the lease over, and stays
	// the same while its holder renews it. Agents refuse instructions carrying
	// a term lower than the highest they have seen.
	Term int64 `json:"term"`
	// Seq goes up by one on every write, renewals included. Standbys tell a
	// renewal from silence by the ETag changing, and an S3 ETag of a
	// single-part upload is the MD5 of the content, so two writes of the same
	// bytes would look like no write at all. Seq guarantees the bytes differ;
	// RenewedAt cannot, since two renewals may fall in the same second or a
	// clock may be stepped back.
	Seq int64 `json:"seq"`
	// Holder is the stable identity of the server holding the lease
	// (SERVER_ID, by default the hostname).
	Holder string `json:"holder"`
	// URL is where Agents and caractl reach the holder: its underlay address
	// (ADR-0010).
	URL string `json:"url"`
	// RenewedAt is when the holder last wrote the lease, by its own clock. It
	// is for people reading the file. No decision reads it, because clocks on
	// different machines need not agree (ADR-0037).
	RenewedAt time.Time `json:"renewedAt"`
}

// ErrInvalidRecord means a lease record is not one this package can act on.
var ErrInvalidRecord = errors.New("leaderlease: invalid lease record")

// Validate reports whether r is a complete lease of the current version.
func (r Record) Validate() error {
	if r.Version != CurrentVersion {
		return fmt.Errorf("%w: version %d, this server understands %d", ErrInvalidRecord, r.Version, CurrentVersion)
	}
	if r.Term < 1 {
		return fmt.Errorf("%w: term %d, terms start at 1", ErrInvalidRecord, r.Term)
	}
	if r.Seq < 1 {
		return fmt.Errorf("%w: seq %d, writes are counted from 1", ErrInvalidRecord, r.Seq)
	}
	if r.Holder == "" {
		return fmt.Errorf("%w: no holder", ErrInvalidRecord)
	}
	if err := ValidateURL(r.URL); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRecord, err)
	}
	return nil
}

// ValidateURL reports whether raw is an absolute http or https URL with a
// host, the only form a lease's URL may take.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http:// or https:// URL", raw)
	}
	return nil
}
