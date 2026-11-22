//go:build e2e

package leaderlease

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"NYCU-SDC/caravanserai/internal/objectstore"
	"NYCU-SDC/caravanserai/internal/server/leaderlease"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newObjects(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	objects, err := objectstore.NewMinioStore(objectstore.MinioConfig{
		Endpoint:  shared.endpoint,
		Bucket:    testhelper.MinioBucket,
		AccessKey: testhelper.MinioAccessKey,
		SecretKey: testhelper.MinioSecretKey,
	})
	require.NoError(t, err)
	return objects
}

// newLeaseStore keeps each test's lease under its own key so the tests do not
// see each other's writes.
func newLeaseStore(t *testing.T) *leaderlease.Store {
	t.Helper()
	store, err := leaderlease.NewStore(newObjects(t), fmt.Sprintf("test/%s/lease.json", t.Name()))
	require.NoError(t, err)
	return store
}

// TestConditionalPutAgainstMinio checks the four outcomes the lease relies on,
// at the objectstore level.
func TestConditionalPutAgainstMinio(t *testing.T) {
	objects := newObjects(t)
	ctx := context.Background()
	const key = "test/conditional-put/object"
	put := func(body string, opts objectstore.PutOptions) (objectstore.ObjectMeta, error) {
		opts.Size = int64(len(body))
		return objects.Put(ctx, key, bytes.NewReader([]byte(body)), opts)
	}

	first, err := put("v1", objectstore.PutOptions{IfAbsent: true})
	require.NoError(t, err, "if-absent on a missing key creates it")

	_, err = put("v1-again", objectstore.PutOptions{IfAbsent: true})
	assert.ErrorIs(t, err, objectstore.ErrPreconditionFailed, "if-absent on an existing key fails")

	second, err := put("v2", objectstore.PutOptions{IfMatch: first.ETag})
	require.NoError(t, err, "if-match with the current ETag replaces it")
	assert.NotEqual(t, first.ETag, second.ETag)

	_, err = put("v3", objectstore.PutOptions{IfMatch: first.ETag})
	assert.ErrorIs(t, err, objectstore.ErrPreconditionFailed, "if-match with a stale ETag fails")

	meta, err := objects.Head(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, second.ETag, meta.ETag, "the failed writes left the object as it was")
}

func TestLeaseLifecycleAgainstMinio(t *testing.T) {
	store := newLeaseStore(t)
	ctx := context.Background()

	_, err := store.Read(ctx)
	require.ErrorIs(t, err, leaderlease.ErrNoLease)

	created, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)
	_, err = store.Create(ctx, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, leaderlease.ErrLost)

	seen, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, created, seen, "Read returns what Create wrote, with the same ETag")

	// Renewals follow one another within the same second here, so only Seq
	// keeps their bytes, and therefore MinIO's ETags, apart.
	held := created
	for range 3 {
		renewed, err := store.Renew(ctx, held)
		require.NoError(t, err)
		assert.NotEqual(t, held.ETag, renewed.ETag, "every renewal changes the ETag standbys watch")
		held = renewed
	}

	_, err = store.TakeOver(ctx, seen, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, leaderlease.ErrLost, "a takeover from a read older than the renewals loses")

	taken, err := store.TakeOver(ctx, held, "cp-2", "http://cp-2:8080")
	require.NoError(t, err)
	assert.Equal(t, int64(2), taken.Record.Term)

	_, err = store.Renew(ctx, held)
	assert.ErrorIs(t, err, leaderlease.ErrLost, "the old holder's renewal loses once it is taken over")

	got, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, taken, got)
}

// TestConcurrentTakeoversAgainstMinio: standbys racing to take over the same
// lease must produce exactly one winner on a real server.
func TestConcurrentTakeoversAgainstMinio(t *testing.T) {
	store := newLeaseStore(t)
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
		assert.ErrorIs(t, err, leaderlease.ErrLost)
	}
	assert.Equal(t, 1, wins)

	got, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Record.Term)
}
