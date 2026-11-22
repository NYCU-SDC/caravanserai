package leaderlease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"NYCU-SDC/caravanserai/internal/objectstore"
)

// maxRecordSize bounds how much of lease.json is read. A real record is a few
// hundred bytes; anything far larger is not a lease this package wrote.
const maxRecordSize = 64 << 10

// ErrNoLease means no lease.json exists yet: no server has ever held the
// lease, or someone deleted it.
var ErrNoLease = errors.New("leaderlease: no lease recorded")

// ErrLost means a conditional write lost: the lease changed (or appeared)
// since the caller read it, so another server wrote first. The caller must
// read the lease again and decide from what it finds; retrying the same write
// can never succeed.
var ErrLost = errors.New("leaderlease: lease changed since it was read")

// Lease is a lease record together with the ETag it was stored with. The
// ETag is what the next conditional write must name, and what a standby
// watches for change.
type Lease struct {
	Record Record
	ETag   string
}

// Store reads and conditionally writes lease.json. Its write methods own the
// rules every write must follow, so no caller can get them wrong: Seq goes up
// on every write, Term stays the same on a renewal and goes up by one on a
// takeover, and each write is conditional on the state its caller read.
type Store struct {
	objects objectstore.ObjectStore
	key     string
	now     func() time.Time
}

// NewStore returns a Store keeping the lease at key in objects. An empty key
// means DefaultKey.
func NewStore(objects objectstore.ObjectStore, key string) (*Store, error) {
	if objects == nil {
		return nil, errors.New("leaderlease: an object store is required")
	}
	if key == "" {
		key = DefaultKey
	}
	return &Store{objects: objects, key: key, now: time.Now}, nil
}

// Key is where the lease is kept.
func (s *Store) Key() string { return s.key }

// Read returns the stored lease. It returns ErrNoLease when there is none,
// and an error wrapping ErrInvalidRecord when the stored lease cannot be
// parsed or is not one this server understands; a caller must not treat that
// as "no lease", since overwriting it could hand the lease to a second
// server.
func (s *Store) Read(ctx context.Context) (Lease, error) {
	body, meta, err := s.objects.Get(ctx, s.key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return Lease{}, ErrNoLease
		}
		return Lease{}, fmt.Errorf("leaderlease: read %s: %w", s.key, err)
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(body, maxRecordSize+1))
	if err != nil {
		return Lease{}, fmt.Errorf("leaderlease: read %s: %w", s.key, err)
	}
	if len(raw) > maxRecordSize {
		return Lease{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalidRecord, s.key, maxRecordSize)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Lease{}, fmt.Errorf("%w: %s: %w", ErrInvalidRecord, s.key, err)
	}
	if err := rec.Validate(); err != nil {
		return Lease{}, err
	}
	if meta.ETag == "" {
		return Lease{}, fmt.Errorf("leaderlease: read %s: the object store reported no ETag", s.key)
	}
	return Lease{Record: rec, ETag: meta.ETag}, nil
}

// Create writes the first lease, term 1, held by holder at url, only if no
// lease exists yet. It returns ErrLost if another server created one first.
func (s *Store) Create(ctx context.Context, holder, url string) (Lease, error) {
	rec := Record{Version: CurrentVersion, Term: 1, Seq: 1, Holder: holder, URL: url}
	return s.write(ctx, rec, objectstore.PutOptions{IfAbsent: true})
}

// Renew rewrites held, the lease this server holds as it last wrote or read
// it, with the same term and holder. It returns ErrLost if the lease changed
// since: another server took it over, and this one must stop.
func (s *Store) Renew(ctx context.Context, held Lease) (Lease, error) {
	rec := held.Record
	rec.Seq++
	return s.replace(ctx, rec, held.ETag)
}

// TakeOver replaces seen, the lease as this server last read it, with one
// term higher held by holder at url. It returns ErrLost if the lease changed
// since seen was read: its holder renewed it, or another standby won.
func (s *Store) TakeOver(ctx context.Context, seen Lease, holder, url string) (Lease, error) {
	rec := seen.Record
	rec.Term++
	rec.Seq++
	rec.Holder = holder
	rec.URL = url
	return s.replace(ctx, rec, seen.ETag)
}

func (s *Store) replace(ctx context.Context, rec Record, etag string) (Lease, error) {
	if etag == "" {
		return Lease{}, errors.New("leaderlease: a conditional write needs the ETag the lease was read with")
	}
	return s.write(ctx, rec, objectstore.PutOptions{IfMatch: etag})
}

func (s *Store) write(ctx context.Context, rec Record, opts objectstore.PutOptions) (Lease, error) {
	rec.RenewedAt = s.now().UTC().Truncate(time.Second)
	if err := rec.Validate(); err != nil {
		return Lease{}, err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return Lease{}, fmt.Errorf("leaderlease: encode lease: %w", err)
	}
	opts.ContentType = "application/json"
	opts.Size = int64(len(raw))
	meta, err := s.objects.Put(ctx, s.key, bytes.NewReader(raw), opts)
	if err != nil {
		if errors.Is(err, objectstore.ErrPreconditionFailed) {
			return Lease{}, ErrLost
		}
		return Lease{}, fmt.Errorf("leaderlease: write %s: %w", s.key, err)
	}
	if meta.ETag == "" {
		return Lease{}, fmt.Errorf("leaderlease: write %s: the object store reported no ETag", s.key)
	}
	return Lease{Record: rec, ETag: meta.ETag}, nil
}
