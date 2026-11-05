package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const retentionUID = "uid-current"

// seedGeneration stores a generation as a finished run would have left it.
func seedGeneration(t *testing.T, store *fakeStore, id, uid string) {
	t.Helper()
	ctx := context.Background()
	archive, err := ArchiveKey("default", "blog", id, "db-data")
	require.NoError(t, err)
	_, err = store.Put(ctx, archive, strings.NewReader("archive"), objectstore.PutOptions{Size: 7})
	require.NoError(t, err)

	m := BuildManifest(id, "default", "blog", "node-a", "1.0.0", 7, time.Now(), nil)
	m.ProjectUID = uid
	body, err := m.Marshal()
	require.NoError(t, err)
	manifest, err := ManifestKey("default", "blog", id)
	require.NoError(t, err)
	_, err = store.Put(ctx, manifest, strings.NewReader(string(body)), objectstore.PutOptions{Size: int64(len(body))})
	require.NoError(t, err)
}

func seedID(day int) string { return fmt.Sprintf("202607%02dT000000Z-aaaaaaaa", day) }

func generationsIn(t *testing.T, store *fakeStore) []string {
	t.Helper()
	objects, err := store.List(context.Background(), "")
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, o := range objects {
		parts := strings.Split(o.Key, "/snapshots/")
		require.Len(t, parts, 2, o.Key)
		seen[strings.SplitN(parts[1], "/", 2)[0]] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func retentionHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "db-data")
	h.project.ObjectMeta.UID = retentionUID
	return h
}

func TestRunKeepsTheNewestGenerations(t *testing.T) {
	h := retentionHarness(t)
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	committed := h.committer.committed()[0]
	want := []string{committed}
	for day := 5; day <= RetainGenerations+3; day++ {
		want = append(want, seedID(day))
	}
	assert.Len(t, want, RetainGenerations, "the new generation counts towards the limit")
	assert.ElementsMatch(t, want, generationsIn(t, h.store))
}

func TestRunNeverPrunesTheCommittedGeneration(t *testing.T) {
	h := retentionHarness(t)
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}
	// The commit below replaces this, but until it is read back the Project's
	// status is the only record of what the server may restore from.
	h.project.Status.Recovery = &v1.ProjectRecoveryStatus{CommittedBackup: &v1.CommittedBackup{BackupID: seedID(1)}}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Contains(t, generationsIn(t, h.store), seedID(1))
}

func TestRunLeavesAnEarlierLifetimesGenerations(t *testing.T) {
	h := retentionHarness(t)
	seedGeneration(t, h.store, seedID(1), "uid-earlier")
	seedGeneration(t, h.store, seedID(2), "") // written before manifests named a UID
	for day := 3; day <= RetainGenerations+5; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	got := generationsIn(t, h.store)
	assert.Contains(t, got, seedID(1), "a same-name Project must not delete another lifetime's backups")
	assert.Contains(t, got, seedID(2), "a generation with no UID cannot be proven to be this Project's")
	assert.NotContains(t, got, seedID(3))
}

func TestRunLeavesEverythingWhenTheProjectHasNoUID(t *testing.T) {
	h := newHarness(t, "db-data")
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), "")
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Len(t, generationsIn(t, h.store), RetainGenerations+4)
}

func TestRunPrunesAbandonedGenerationsOlderThanTheKeptOnes(t *testing.T) {
	h := retentionHarness(t)
	for day := 2; day <= RetainGenerations+2; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}
	// Uploads with no manifest: one old, one newer than what is kept, which may
	// be a run still in progress on a Node that lost the assignment.
	for _, id := range []string{seedID(1), seedID(RetainGenerations + 2)[:8] + "T120000Z-bbbbbbbb"} {
		archive, err := ArchiveKey("default", "blog", id, "db-data")
		require.NoError(t, err)
		_, err = h.store.Put(context.Background(), archive, strings.NewReader("partial"), objectstore.PutOptions{Size: 7})
		require.NoError(t, err)
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	got := generationsIn(t, h.store)
	assert.NotContains(t, got, seedID(1))
	assert.Contains(t, got, seedID(RetainGenerations + 2)[:8]+"T120000Z-bbbbbbbb")
}

func TestRunPrunesNothingAfterARefusedCommit(t *testing.T) {
	h := retentionHarness(t)
	h.committer.err = errors.New("stale assignment")
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}

	require.Error(t, h.runner.Run(context.Background(), h.project))

	assert.Contains(t, generationsIn(t, h.store), seedID(1),
		"an Agent that lost the assignment must not delete the new owner's backups")
}

// failingDeleteStore fails every delete, and records what the containers were
// doing when it was asked.
type failingDeleteStore struct {
	*fakeStore
	containers *fakeContainers
	err        error
	seen       []string
}

func (s *failingDeleteStore) Delete(ctx context.Context, key string) error {
	s.seen = s.containers.history()
	if s.err != nil {
		return s.err
	}
	return s.fakeStore.Delete(ctx, key)
}

func TestRunSucceedsWhenPruningFails(t *testing.T) {
	h := retentionHarness(t)
	h.runner.store = &failingDeleteStore{fakeStore: h.store, containers: h.containers, err: errors.New("bucket unavailable")}
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project), "the backup itself succeeded")
	assert.Len(t, generationsIn(t, h.store), RetainGenerations+4)
}

func TestRunPrunesOnlyAfterTheServiceIsBack(t *testing.T) {
	h := retentionHarness(t)
	store := &failingDeleteStore{fakeStore: h.store, containers: h.containers}
	h.runner.store = store
	for day := 1; day <= RetainGenerations+3; day++ {
		seedGeneration(t, h.store, seedID(day), retentionUID)
	}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Equal(t, []string{"stop", "start"}, store.seen, "pruning must not lengthen the downtime")
}

// A generation interrupted between its two deletes has lost its manifest, so it
// reads as abandoned and the next run finishes the job. Archives first would
// leave a manifest naming archives that no longer exist.
func TestPruneRemovesTheManifestFirst(t *testing.T) {
	h := retentionHarness(t)
	var order []string
	h.runner.store = &orderRecordingStore{fakeStore: h.store, order: &order}
	seedGeneration(t, h.store, seedID(1), retentionUID)

	h.runner.deleteGeneration(context.Background(), "default/blog", generationObjects{
		id:   seedID(1),
		keys: h.store.keys(),
	}, h.runner.logger)

	require.Len(t, order, 2)
	assert.True(t, strings.HasSuffix(order[0], "manifest.json"), order[0])
}

type orderRecordingStore struct {
	*fakeStore
	order *[]string
}

func (s *orderRecordingStore) Delete(ctx context.Context, key string) error {
	*s.order = append(*s.order, key)
	return s.fakeStore.Delete(ctx, key)
}
