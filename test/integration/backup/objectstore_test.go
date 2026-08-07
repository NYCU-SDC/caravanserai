//go:build e2e

package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"NYCU-SDC/caravanserai/internal/objectstore"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStore builds an ObjectStore pointed at the shared MinIO container,
// exactly as the agent builds one from its config.
func newStore(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	store, err := objectstore.NewMinioStore(objectstore.MinioConfig{
		Endpoint:  shared.endpoint,
		Bucket:    testhelper.MinioBucket,
		AccessKey: testhelper.MinioAccessKey,
		SecretKey: testhelper.MinioSecretKey,
	})
	require.NoError(t, err)
	return store
}

func put(t *testing.T, store objectstore.ObjectStore, key string, body []byte) objectstore.ObjectMeta {
	t.Helper()
	meta, err := store.Put(context.Background(), key, bytes.NewReader(body), objectstore.PutOptions{
		ContentType: "application/octet-stream",
		Size:        int64(len(body)),
	})
	require.NoError(t, err)
	return meta
}

// TestObjectStoreRoundTrip exercises every operation the backup and restore
// flows depend on against a real S3-compatible server, confirming the
// behaviour the unit tests' fakes assume.
func TestObjectStoreRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	const key = "cara/v1/roundtrip/hello.txt"
	body := []byte("hello from cara")

	t.Run("Put reports the stored size", func(t *testing.T) {
		meta := put(t, store, key, body)
		assert.Equal(t, int64(len(body)), meta.Size)
	})

	t.Run("Head returns metadata without the body", func(t *testing.T) {
		meta, err := store.Head(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, int64(len(body)), meta.Size)
		assert.NotEmpty(t, meta.ETag)
	})

	t.Run("Get returns the exact bytes", func(t *testing.T) {
		rc, meta, err := store.Get(ctx, key)
		require.NoError(t, err)
		defer func() { _ = rc.Close() }()

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		assert.Equal(t, int64(len(body)), meta.Size)
	})

	t.Run("List finds the key by prefix", func(t *testing.T) {
		metas, err := store.List(ctx, "cara/v1/roundtrip/")
		require.NoError(t, err)

		var found bool
		for _, m := range metas {
			if m.Key == key {
				found = true
				assert.Equal(t, int64(len(body)), m.Size)
			}
		}
		assert.True(t, found, "the uploaded key should appear under its prefix")
	})

	t.Run("Delete removes the object", func(t *testing.T) {
		require.NoError(t, store.Delete(ctx, key))
		_, err := store.Head(ctx, key)
		assert.ErrorIs(t, err, objectstore.ErrNotFound)
	})
}

// TestObjectStoreNotFoundSemantics pins the distinction the restore path
// depends on: a missing key must be reported as ErrNotFound rather than an
// opaque provider error, because "no backup yet" and "the store is broken"
// lead to opposite decisions.
func TestObjectStoreNotFoundSemantics(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	const missing = "cara/v1/definitely/not/here.json"

	t.Run("Head on a missing key", func(t *testing.T) {
		_, err := store.Head(ctx, missing)
		require.Error(t, err)
		assert.True(t, errors.Is(err, objectstore.ErrNotFound),
			"got %v, want an error wrapping ErrNotFound", err)
	})

	t.Run("Get on a missing key", func(t *testing.T) {
		_, _, err := store.Get(ctx, missing)
		require.Error(t, err)
		assert.True(t, errors.Is(err, objectstore.ErrNotFound),
			"got %v, want an error wrapping ErrNotFound", err)
	})

	t.Run("Delete on a missing key is a no-op", func(t *testing.T) {
		// S3 DELETE is idempotent; the backup cleanup path relies on this.
		assert.NoError(t, store.Delete(ctx, missing))
	})
}

// TestObjectStoreOverwrite confirms latest.json can be replaced in place,
// which is what makes it the single mutable object in the layout.
func TestObjectStoreOverwrite(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	const key = "cara/v1/overwrite/latest.json"

	put(t, store, key, []byte(`{"backupID":"first"}`))
	put(t, store, key, []byte(`{"backupID":"second"}`))

	rc, _, err := store.Get(ctx, key)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.JSONEq(t, `{"backupID":"second"}`, string(got))

	require.NoError(t, store.Delete(ctx, key))
}
