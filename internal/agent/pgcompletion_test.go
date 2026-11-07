package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/pgbackup"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStore is an object store in memory that records the order of writes.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
	onPut   func(key string)
}

func (m *memStore) Put(_ context.Context, key string, body io.Reader, _ objectstore.PutOptions) (objectstore.ObjectMeta, error) {
	if m.putErr != nil {
		return objectstore.ObjectMeta{}, m.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return objectstore.ObjectMeta{}, err
	}
	m.mu.Lock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[key] = data
	m.mu.Unlock()
	if m.onPut != nil {
		m.onPut(key)
	}
	return objectstore.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, objectstore.ObjectMeta{}, objectstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), objectstore.ObjectMeta{Key: key}, nil
}

func (m *memStore) Head(context.Context, string) (objectstore.ObjectMeta, error) {
	return objectstore.ObjectMeta{}, objectstore.ErrNotFound
}
func (m *memStore) List(context.Context, string) ([]objectstore.ObjectMeta, error) { return nil, nil }
func (m *memStore) Delete(context.Context, string) error                           { return nil }

var completionItems = []pgbackup.Item{{
	Volume: "pgdata", Stanza: "uid-1-pgdata", Label: "20261017-030000F", Type: pgbackup.TypeFull, PGMajor: 16,
	Started: time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC), Stopped: time.Date(2026, 10, 17, 3, 1, 0, 0, time.UTC),
}}

func runningPgProject() *v1.Project {
	p := pgProject()
	p.Status.Phase = v1.ProjectPhaseRunning
	return p
}

// The manifest is in the store before the server hears of the backup, under
// the lifetime's UID, and the report carries the full fence.
func TestCompletionWritesTheManifestBeforeReporting(t *testing.T) {
	gs, client := newGateServer(t, runningPgProject())
	store := &memStore{}
	key := v1.BackupManifestKey("uid-1", "20261017T030000Z-0000000a")
	store.onPut = func(string) {
		gs.mu.Lock()
		defer gs.mu.Unlock()
		assert.Empty(t, gs.completions, "nothing is reported before the manifest is written")
	}
	rec := newCompletionRecorder(client, store, "v-test")

	require.NoError(t, rec.Record(context.Background(), runningPgProject(), "20261017T030000Z-0000000a", completionItems))

	var m pgbackup.Manifest
	require.NoError(t, json.Unmarshal(store.objects[key], &m))
	assert.Equal(t, 2, m.FormatVersion)
	assert.Equal(t, "uid-1", m.ProjectUID)
	assert.Equal(t, gateNode, m.NodeRef)
	assert.Equal(t, int64(1), m.AssignmentGeneration)
	require.Len(t, m.Items, 1)
	assert.Equal(t, pgbackup.ManifestItem{
		Volume: "pgdata", Strategy: v1.BackupStrategyPostgresDifferential, Stanza: "uid-1-pgdata",
		BackupLabel: "20261017-030000F", BackupType: "full", PGMajor: 16,
		StartedAt: completionItems[0].Started, StoppedAt: completionItems[0].Stopped,
	}, m.Items[0])

	require.Len(t, gs.completions, 1)
	assert.Equal(t, AssignmentFence{UID: "uid-1", NodeRef: gateNode, Generation: 1}, gs.completions[0].Fence)
	assert.Equal(t, []v1.BackupCompletionItem{{Volume: "pgdata", Stanza: "uid-1-pgdata", Label: "20261017-030000F", Type: "full"}},
		gs.completions[0].Items)
}

// Without a manifest there is nothing to report.
func TestCompletionIsNotReportedWithoutAManifest(t *testing.T) {
	gs, client := newGateServer(t, runningPgProject())
	rec := newCompletionRecorder(client, &memStore{putErr: errors.New("bucket unreachable")}, "v-test")
	assert.Error(t, rec.Record(context.Background(), runningPgProject(), "20261017T030000Z-0000000a", completionItems))
	assert.Empty(t, gs.completions)
}

// A refused fence is final; a server error is retried with the same backupID.
func TestCompletionRetriesOnlyWhatMaySucceed(t *testing.T) {
	for _, tc := range []struct {
		status   int
		attempts int
		want     error
	}{
		{http.StatusConflict, 1, ErrStaleAssignment},
		{http.StatusInternalServerError, completionAttempts, nil},
	} {
		gs, client := newGateServer(t, runningPgProject())
		gs.completionStatus = tc.status
		rec := newCompletionRecorder(client, &memStore{}, "v-test")
		rec.backoff = time.Millisecond
		err := rec.Record(context.Background(), runningPgProject(), "20261017T030000Z-0000000a", completionItems)
		require.Error(t, err)
		if tc.want != nil {
			assert.ErrorIs(t, err, tc.want)
		}
		require.Len(t, gs.completions, tc.attempts, "status %d", tc.status)
		for _, c := range gs.completions {
			assert.Equal(t, "20261017T030000Z-0000000a", c.BackupID)
		}
	}
}
