//go:build e2e

// Package external runs the leader lease's conditional-write checks against
// an S3-compatible store outside the test run, such as the Cloudflare R2
// bucket production will use. It is skipped unless LEASE_TEST_S3_ENDPOINT is
// set, so CI and `make test-integration` without credentials pass over it.
//
//	set -a; . ~/.config/cara/r2-test.env; set +a
//	go test -tags e2e -v ./test/integration/leaderlease/external/
//
// LEASE_TEST_S3_ENDPOINT  https://... (TLS follows the scheme)
// LEASE_TEST_S3_BUCKET    an existing bucket the credentials can read and write
// LEASE_TEST_S3_REGION    optional; R2 uses "auto"
// LEASE_TEST_S3_ACCESS_KEY, LEASE_TEST_S3_SECRET_KEY
//
// Every object is written under a key unique to the run and deleted at the
// end. Use a test bucket: these tests never touch any other key, but they do
// write to the bucket they are given.
package external

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/objectstore"
	"NYCU-SDC/caravanserai/internal/server/leaderlease"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runPrefix keeps this run's objects apart from any earlier or concurrent run.
var runPrefix = fmt.Sprintf("cara-test/leaderlease/%d", time.Now().UnixNano())

func newObjects(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	endpoint := os.Getenv("LEASE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("LEASE_TEST_S3_ENDPOINT is not set; skipping the external S3 checks")
	}
	scheme, host, ok := strings.Cut(endpoint, "://")
	require.True(t, ok && (scheme == "http" || scheme == "https"), "LEASE_TEST_S3_ENDPOINT must start with http:// or https://")
	objects, err := objectstore.NewMinioStore(objectstore.MinioConfig{
		Endpoint:  strings.TrimSuffix(host, "/"),
		Bucket:    os.Getenv("LEASE_TEST_S3_BUCKET"),
		Region:    os.Getenv("LEASE_TEST_S3_REGION"),
		Secure:    scheme == "https",
		AccessKey: os.Getenv("LEASE_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("LEASE_TEST_S3_SECRET_KEY"),
	})
	require.NoError(t, err)
	return objects
}

// cleanup deletes key when the test ends, so the bucket is left as it was.
func cleanup(t *testing.T, objects objectstore.ObjectStore, key string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := objects.Delete(ctx, key); err != nil {
			t.Logf("could not delete %s: %v", key, err)
		}
	})
}

func TestExternalConditionalPut(t *testing.T) {
	objects := newObjects(t)
	ctx := context.Background()
	key := runPrefix + "/conditional-put"
	cleanup(t, objects, key)
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

	_, err = put("v2", objectstore.PutOptions{IfMatch: second.ETag})
	require.NoError(t, err)
	meta, err := objects.Head(ctx, key)
	require.NoError(t, err)
	t.Logf("same bytes rewritten: ETag before %s, after %s", second.ETag, meta.ETag)
}

func TestExternalLeaseLifecycle(t *testing.T) {
	objects := newObjects(t)
	ctx := context.Background()
	key := runPrefix + "/lifecycle.json"
	cleanup(t, objects, key)
	store, err := leaderlease.NewStore(objects, key)
	require.NoError(t, err)

	created, err := store.Create(ctx, "cp-1", "http://cp-1:8080")
	require.NoError(t, err)
	_, err = store.Create(ctx, "cp-2", "http://cp-2:8080")
	assert.ErrorIs(t, err, leaderlease.ErrLost)

	seen, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, created, seen, "Read returns what Create wrote, with the same ETag")

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
	_, err = store.Renew(ctx, held)
	assert.ErrorIs(t, err, leaderlease.ErrLost, "the old holder's renewal loses once it is taken over")

	got, err := store.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, taken, got)
}

func TestExternalConcurrentTakeovers(t *testing.T) {
	objects := newObjects(t)
	ctx := context.Background()
	key := runPrefix + "/concurrent.json"
	cleanup(t, objects, key)
	store, err := leaderlease.NewStore(objects, key)
	require.NoError(t, err)
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

	wins, lost := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case assert.ErrorIs(t, err, leaderlease.ErrLost):
			lost++
		}
	}
	t.Logf("%d concurrent takeovers from one read: %d won, %d lost", racers, wins, lost)
	assert.Equal(t, 1, wins)
}
