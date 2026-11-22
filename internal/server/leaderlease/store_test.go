package leaderlease

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeObjects is an in-memory ObjectStore that behaves like an S3 store where
// the lease depends on it: PutOptions.IfMatch and IfAbsent are honoured, and
// the ETag is the MD5 of the content, as S3 reports for a single-part upload.
// The second matters: writing the same bytes again leaves the ETag unchanged,
// which an election watching for ETag changes must not mistake for a renewal.
// test/integration/leaderlease checks the same behaviour against MinIO.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{objects: map[string][]byte{}}
}

func etagOf(raw []byte) string {
	sum := md5.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func (f *fakeObjects) Put(_ context.Context, key string, body io.Reader, opts objectstore.PutOptions) (objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return objectstore.ObjectMeta{}, f.putErr
	}
	current, exists := f.objects[key]
	if opts.IfAbsent && exists {
		return objectstore.ObjectMeta{}, fmt.Errorf("put %q: %w", key, objectstore.ErrPreconditionFailed)
	}
	if opts.IfMatch != "" && (!exists || etagOf(current) != opts.IfMatch) {
		return objectstore.ObjectMeta{}, fmt.Errorf("put %q: %w", key, objectstore.ErrPreconditionFailed)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return objectstore.ObjectMeta{}, err
	}
	f.objects[key] = raw
	return objectstore.ObjectMeta{Key: key, ETag: etagOf(raw), Size: int64(len(raw))}, nil
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.objects[key]
	if !ok {
		return nil, objectstore.ObjectMeta{}, fmt.Errorf("get %q: %w", key, objectstore.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(raw)), objectstore.ObjectMeta{Key: key, ETag: etagOf(raw), Size: int64(len(raw))}, nil
}

func (f *fakeObjects) Head(ctx context.Context, key string) (objectstore.ObjectMeta, error) {
	rc, meta, err := f.Get(ctx, key)
	if err == nil {
		_ = rc.Close()
	}
	return meta, err
}

func (f *fakeObjects) List(context.Context, string) ([]objectstore.ObjectMeta, error) {
	return nil, errors.New("not used")
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

// set stores raw bytes as the lease, as something other than Store would.
func (f *fakeObjects) set(key, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = []byte(raw)
}

// newTestStore returns a Store whose clock is frozen, so every write happens
// "in the same second": the worst case for telling writes apart.
func newTestStore(t *testing.T) (*Store, *fakeObjects) {
	t.Helper()
	objects := newFakeObjects()
	store, err := NewStore(objects, "")
	require.NoError(t, err)
	frozen := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return frozen }
	return store, objects
}

func TestNewStoreDefaultsTheKey(t *testing.T) {
	store, _ := newTestStore(t)
	assert.Equal(t, DefaultKey, store.Key())

	_, err := NewStore(nil, "")
	assert.Error(t, err)
}

func TestReadWithoutALease(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.Read(context.Background())
	assert.ErrorIs(t, err, ErrNoLease)
}

func TestCreateOnlyWhenAbsent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Record.Term)
	assert.Equal(t, int64(1), created.Record.Seq)

	got, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, created, got, "Read returns what Create wrote, with the same ETag")

	_, err = store.Create(ctx, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, ErrLost, "a second server creating the first lease loses")

	got, err = store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "cp-1", got.Record.Holder, "the losing create wrote nothing")
}

// TestEveryRenewalChangesTheETag guards the property standbys rely on to tell
// a renewal from silence. With the clock frozen, a renewal that only rewrote
// the same fields would store the same bytes, and S3 would report the same
// ETag.
func TestEveryRenewalChangesTheETag(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	held, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)

	seen := map[string]bool{held.ETag: true}
	for range 5 {
		held, err = store.Renew(ctx, held)
		require.NoError(t, err)
		assert.False(t, seen[held.ETag], "renewal %d reused an earlier ETag", held.Record.Seq)
		seen[held.ETag] = true
		assert.Equal(t, int64(1), held.Record.Term, "a renewal keeps the term")
		assert.Equal(t, "cp-1", held.Record.Holder)
	}
	assert.Equal(t, int64(6), held.Record.Seq)
}

func TestRenewLosesOnceTheLeaseChanged(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	held, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)

	taken, err := store.TakeOver(ctx, held, "cp-2", "http://cp-2:8080")
	require.NoError(t, err)
	assert.Equal(t, int64(2), taken.Record.Term)
	assert.Equal(t, int64(2), taken.Record.Seq)
	assert.Equal(t, "http://cp-2:8080", taken.Record.URL)

	_, err = store.Renew(ctx, held)
	assert.ErrorIs(t, err, ErrLost, "the old holder's renewal loses: it must stop")
}

func TestTakeOverLosesToARenewal(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seen, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)

	_, err = store.Renew(ctx, seen)
	require.NoError(t, err)

	_, err = store.TakeOver(ctx, seen, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, ErrLost, "a takeover from a read older than the renewal loses")
}

func TestWritesLoseAfterTheLeaseWasDeleted(t *testing.T) {
	store, objects := newTestStore(t)
	ctx := context.Background()
	held, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)
	require.NoError(t, objects.Delete(ctx, DefaultKey))

	_, err = store.Renew(ctx, held)
	assert.ErrorIs(t, err, ErrLost)
	_, err = store.TakeOver(ctx, held, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, ErrLost)
}

func TestConditionalWritesNeedAnETag(t *testing.T) {
	store, _ := newTestStore(t)
	held := Lease{Record: Record{Version: CurrentVersion, Term: 1, Seq: 1, Holder: "cp-1", URL: "http://cp-1:8080"}}
	_, err := store.Renew(context.Background(), held)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrLost)
}

// TestConcurrentTakeoversHaveOneWinner is the property the election rests on:
// standbys that read the same lease and race to take it over cannot both win.
func TestConcurrentTakeoversHaveOneWinner(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	seen, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)

	const racers = 20
	results := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			holder := fmt.Sprintf("cp-%d", i+2)
			_, results[i] = store.TakeOver(ctx, seen, holder, "http://"+holder+":8080")
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
			continue
		}
		assert.ErrorIs(t, err, ErrLost)
	}
	assert.Equal(t, 1, wins)
}

func TestReadRefusesWhatItCannotActOn(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "not JSON", raw: "not json"},
		{name: "a newer version", raw: `{"version":2,"term":3,"seq":4,"holder":"cp-1","url":"http://cp-1:8080"}`},
		{name: "no term", raw: `{"version":1,"seq":4,"holder":"cp-1","url":"http://cp-1:8080"}`},
		{name: "no seq", raw: `{"version":1,"term":3,"holder":"cp-1","url":"http://cp-1:8080"}`},
		{name: "no holder", raw: `{"version":1,"term":3,"seq":4,"url":"http://cp-1:8080"}`},
		{name: "a URL without a scheme", raw: `{"version":1,"term":3,"seq":4,"holder":"cp-1","url":"cp-1:8080"}`},
		{name: "far too large", raw: `{"version":1,"term":3,"seq":4,"holder":"` + strings.Repeat("x", maxRecordSize) + `","url":"http://cp-1:8080"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, objects := newTestStore(t)
			objects.set(DefaultKey, tt.raw)
			_, err := store.Read(context.Background())
			assert.ErrorIs(t, err, ErrInvalidRecord)
			assert.NotErrorIs(t, err, ErrNoLease, "an unreadable lease must never look like no lease")
		})
	}
}

func TestWriteRefusesAnInvalidRecord(t *testing.T) {
	store, objects := newTestStore(t)
	_, err := store.Create(context.Background(), "cp-1", "")
	assert.ErrorIs(t, err, ErrInvalidRecord)
	assert.Empty(t, objects.objects, "nothing was written")
}

func TestOtherWriteErrorsAreNotALostRace(t *testing.T) {
	store, objects := newTestStore(t)
	objects.putErr = errors.New("connection refused")
	_, err := store.Create(context.Background(), "cp-1", "http://cp-1:8080")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrLost, "an unreachable store must be retried, not read as another server winning")
}
