package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeContainers struct {
	mu      sync.Mutex
	calls   []string // "stop" / "start", in order
	stopErr error
}

func (f *fakeContainers) StopProject(context.Context, *v1.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	return f.stopErr
}

func (f *fakeContainers) StartProject(context.Context, *v1.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	return nil
}

func (f *fakeContainers) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeStore is an in-memory ObjectStore that records the order of writes and
// can be told to fail on a particular key substring.
type fakeStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	putOrder  []string
	failOnKey string
	// truncate makes Put store fewer bytes than advertised, simulating an
	// upload that the Head verification must catch.
	truncateOnKey string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (f *fakeStore) Put(_ context.Context, key string, body io.Reader, _ objectstore.PutOptions) (objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failOnKey != "" && strings.Contains(key, f.failOnKey) {
		return objectstore.ObjectMeta{}, fmt.Errorf("simulated upload failure for %q", key)
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return objectstore.ObjectMeta{}, err
	}
	if f.truncateOnKey != "" && strings.Contains(key, f.truncateOnKey) && len(data) > 0 {
		data = data[:len(data)-1]
	}

	f.objects[key] = data
	f.putOrder = append(f.putOrder, key)
	return objectstore.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeStore) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, objectstore.ObjectMeta{}, objectstore.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(data))), objectstore.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeStore) Head(_ context.Context, key string) (objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return objectstore.ObjectMeta{}, objectstore.ErrNotFound
	}
	return objectstore.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]objectstore.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []objectstore.ObjectMeta
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, objectstore.ObjectMeta{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeStore) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putOrder...)
}

type fakeOwnership struct{ result Ownership }

func (f fakeOwnership) Resolve(context.Context, ResourceKey) Ownership { return f.result }

type fakeConditions struct {
	mu     sync.Mutex
	calls  []string
	setErr error
}

func (f *fakeConditions) SetMaintenance(context.Context, ResourceKey, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "set")
	return f.setErr
}

func (f *fakeConditions) ClearMaintenance(context.Context, ResourceKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "clear")
	return nil
}

func (f *fakeConditions) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fakeRoutes struct {
	mu        sync.Mutex
	refreshes int
}

func (f *fakeRoutes) Refresh(context.Context, *v1.Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
}

func (f *fakeRoutes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

// ── harness ──────────────────────────────────────────────────────────────────

type harness struct {
	runner     *Runner
	containers *fakeContainers
	store      *fakeStore
	conditions *fakeConditions
	routes     *fakeRoutes
	dataRoot   string
	project    *v1.Project
}

// newHarness builds a Runner over temp directories with the given Managed
// volume names, each pre-populated with a file so the archives are non-empty.
func newHarness(t *testing.T, volumeNames ...string) *harness {
	t.Helper()

	dataRoot := t.TempDir()
	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", ResourceVersion: 7},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
			Backup:   &v1.ProjectBackupConfig{Interval: "168h"},
		},
	}

	for _, name := range volumeNames {
		project.Spec.Volumes = append(project.Spec.Volumes, v1.VolumeDef{
			Name: name, Type: v1.VolumeTypeManaged,
		})
		dir := filepath.Join(dataRoot, "volumes", "default", "blog", name, "data")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "content.txt"), []byte("data for "+name), 0o600))
	}

	h := &harness{
		containers: &fakeContainers{},
		store:      newFakeStore(),
		conditions: &fakeConditions{},
		routes:     &fakeRoutes{},
		dataRoot:   dataRoot,
		project:    project,
	}

	h.runner = NewRunner(
		NewCoordinator(), h.containers, h.store,
		fakeOwnership{result: OwnershipRetained}, h.conditions, h.routes,
		Config{DataRoot: dataRoot, NodeName: "node-a", CaraVersion: "1.0.0"},
		zap.NewNop(),
	)
	h.runner.now = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }

	return h
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestRunSkipsProjectWithoutManagedVolumes(t *testing.T) {
	h := newHarness(t)
	h.project.Spec.Volumes = []v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}

	err := h.runner.Run(context.Background(), h.project)

	assert.ErrorIs(t, err, ErrNoManagedVolumes)
	assert.Empty(t, h.containers.history(), "containers must never be touched when there is nothing to back up")
	assert.Empty(t, h.store.keys())
}

func TestRunFailsClosedWithoutObjectStore(t *testing.T) {
	// A Project whose data is meant to be backed up must never run silently
	// unbacked, and finding out must not cost downtime.
	h := newHarness(t, "db-data")
	h.runner.store = nil

	err := h.runner.Run(context.Background(), h.project)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no object store configured")
	assert.Empty(t, h.containers.history(), "containers must not be stopped when the backup cannot proceed")
}

func TestRunAbortsBeforeStoppingWhenDiskIsFull(t *testing.T) {
	h := newHarness(t, "db-data")
	h.runner.cfg.MinFreeBytes = 1 << 40
	h.runner.freeBytes = func(string) (uint64, error) { return 1024, nil }

	err := h.runner.Run(context.Background(), h.project)

	assert.ErrorIs(t, err, ErrInsufficientDiskSpace)
	assert.Empty(t, h.containers.history(), "a full disk must cost no downtime")
}

func TestRunSkipsBusyProject(t *testing.T) {
	h := newHarness(t, "db-data")
	key := ResourceKey{Namespace: "default", Name: "blog"}
	_, ok := h.runner.coordinator.TryClaim(key, OpTerminate)
	require.True(t, ok)

	err := h.runner.Run(context.Background(), h.project)

	assert.NoError(t, err, "losing the claim is not an error, just a skipped tick")
	assert.Empty(t, h.containers.history())
}

func TestRunHappyPathOrdering(t *testing.T) {
	h := newHarness(t, "db-data", "uploads")

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Equal(t, []string{"stop", "start"}, h.containers.history())
	assert.Equal(t, []string{"set", "clear"}, h.conditions.history())
	assert.Equal(t, 1, h.routes.count(), "routes must be refreshed after restart")

	keys := h.store.keys()
	require.Len(t, keys, 4, "two archives, one manifest, one latest pointer")

	// Both archives precede the manifest, and the manifest precedes
	// latest.json — the ordering that keeps latest.json from ever pointing at
	// an incomplete generation.
	assert.Contains(t, keys[0], ".tar.gz")
	assert.Contains(t, keys[1], ".tar.gz")
	assert.Contains(t, keys[2], "manifest.json")
	assert.Contains(t, keys[3], "latest.json")
}

func TestRunWritesConsistentManifestAndLatest(t *testing.T) {
	h := newHarness(t, "db-data", "uploads")

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	latestKey, err := LatestKey("default", "blog")
	require.NoError(t, err)
	rc, _, err := h.store.Get(context.Background(), latestKey)
	require.NoError(t, err)
	latestBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	latest, err := UnmarshalLatest(latestBytes)
	require.NoError(t, err)

	// latest.json must point at a manifest that actually exists.
	require.True(t, h.store.has(latest.ManifestKey))
	rc, _, err = h.store.Get(context.Background(), latest.ManifestKey)
	require.NoError(t, err)
	manifestBytes, err := io.ReadAll(rc)
	require.NoError(t, err)
	manifest, err := UnmarshalManifest(manifestBytes)
	require.NoError(t, err)

	assert.Equal(t, latest.BackupID, manifest.BackupID, "the pointer and manifest must agree on the generation")
	assert.Equal(t, "node-a", manifest.SourceNode)
	assert.Equal(t, int64(7), manifest.ProjectResourceVersion)
	assert.Equal(t, "tar-offline", manifest.BackupMethod)
	require.Len(t, manifest.Volumes, 2)

	// Every archive the manifest references must exist and match its
	// recorded size.
	for _, v := range manifest.Volumes {
		require.True(t, h.store.has(v.ArchiveKey), "manifest references missing archive %q", v.ArchiveKey)
		assert.NotEmpty(t, v.SHA256)
		assert.Positive(t, v.SizeBytes)
	}
}

func TestRunAllVolumesShareOneGeneration(t *testing.T) {
	h := newHarness(t, "db-data", "uploads")

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	manifestKeys := 0
	var generation string
	for _, k := range h.store.keys() {
		if strings.Contains(k, "manifest.json") {
			manifestKeys++
		}
		if strings.Contains(k, "/snapshots/") {
			parts := strings.Split(k, "/snapshots/")
			id := strings.Split(parts[1], "/")[0]
			if generation == "" {
				generation = id
			}
			assert.Equal(t, generation, id, "every object must belong to the same backupID")
		}
	}
	assert.Equal(t, 1, manifestKeys)
}

func TestRunUploadFailureLeavesNoLatestPointer(t *testing.T) {
	// Cross-volume atomicity: if any archive fails, the generation is
	// abandoned. A restore must never pair a new archive with an old one.
	h := newHarness(t, "db-data", "uploads")
	h.store.failOnKey = "uploads.tar.gz"

	err := h.runner.Run(context.Background(), h.project)

	require.Error(t, err)
	latestKey, keyErr := LatestKey("default", "blog")
	require.NoError(t, keyErr)
	assert.False(t, h.store.has(latestKey), "latest.json must not advance when a volume failed")

	for _, k := range h.store.keys() {
		assert.NotContains(t, k, "manifest.json", "no manifest may be written for an incomplete generation")
	}
	assert.Equal(t, []string{"stop", "start"}, h.containers.history(), "containers must come back after a failure")
}

func TestRunDetectsTruncatedUpload(t *testing.T) {
	// The Head check after each upload is what stops a short write from
	// being recorded as a valid archive.
	h := newHarness(t, "db-data")
	h.store.truncateOnKey = "db-data.tar.gz"

	err := h.runner.Run(context.Background(), h.project)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match archive size")
	latestKey, keyErr := LatestKey("default", "blog")
	require.NoError(t, keyErr)
	assert.False(t, h.store.has(latestKey))
}

func TestRunManifestFailureLeavesNoLatestPointer(t *testing.T) {
	h := newHarness(t, "db-data")
	h.store.failOnKey = "manifest.json"

	err := h.runner.Run(context.Background(), h.project)

	require.Error(t, err)
	latestKey, keyErr := LatestKey("default", "blog")
	require.NoError(t, keyErr)
	assert.False(t, h.store.has(latestKey))
	assert.Equal(t, []string{"stop", "start"}, h.containers.history())
}

func TestRunRestartsContainersOnStopFailure(t *testing.T) {
	// StopProject works through services one at a time, so a failure partway
	// leaves some already stopped. Attempting the stop at all obliges the run
	// to attempt the restart, or those containers are stranded.
	h := newHarness(t, "db-data")
	h.containers.stopErr = errors.New("docker unavailable")

	err := h.runner.Run(context.Background(), h.project)

	require.Error(t, err)
	assert.Equal(t, []string{"stop", "start"}, h.containers.history(),
		"a partial stop must still be followed by a restart attempt")
	assert.Equal(t, []string{"set", "clear"}, h.conditions.history())
	assert.False(t, h.runner.coordinator.IsBusy(ResourceKey{Namespace: "default", Name: "blog"}))
}

func TestRunDoesNotRestartWhenOwnershipLost(t *testing.T) {
	for _, ownership := range []Ownership{OwnershipReassigned, OwnershipTerminating, OwnershipLost} {
		t.Run(ownership.String(), func(t *testing.T) {
			h := newHarness(t, "db-data")
			h.runner.ownership = fakeOwnership{result: ownership}

			require.NoError(t, h.runner.Run(context.Background(), h.project))

			assert.Equal(t, []string{"stop"}, h.containers.history(),
				"a Project this node no longer owns must not be restarted here")
			assert.Zero(t, h.routes.count(), "no routes to refresh for a Project we are not running")
		})
	}
}

func TestRunRestartsWhenOwnershipUnknown(t *testing.T) {
	h := newHarness(t, "db-data")
	h.runner.ownership = fakeOwnership{result: OwnershipUnknown}

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Equal(t, []string{"stop", "start"}, h.containers.history(),
		"an unreachable control plane must not become a service outage")
}

func TestRunContinuesWhenMaintenanceConditionFails(t *testing.T) {
	// Correctness rests on the in-process claim, not the server-side
	// condition, so a failed condition write must not abort the backup.
	h := newHarness(t, "db-data")
	h.conditions.setErr = errors.New("server unreachable")

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	assert.Equal(t, []string{"stop", "start"}, h.containers.history())
	latestKey, err := LatestKey("default", "blog")
	require.NoError(t, err)
	assert.True(t, h.store.has(latestKey), "the backup must still complete")
}

func TestRunReleasesClaimOnEveryPath(t *testing.T) {
	key := ResourceKey{Namespace: "default", Name: "blog"}

	t.Run("success", func(t *testing.T) {
		h := newHarness(t, "db-data")
		require.NoError(t, h.runner.Run(context.Background(), h.project))
		assert.False(t, h.runner.coordinator.IsBusy(key))
	})

	t.Run("failure", func(t *testing.T) {
		h := newHarness(t, "db-data")
		h.store.failOnKey = "db-data.tar.gz"
		require.Error(t, h.runner.Run(context.Background(), h.project))
		assert.False(t, h.runner.coordinator.IsBusy(key))
	})
}

func TestRunCleansUpStagingDirectory(t *testing.T) {
	h := newHarness(t, "db-data")

	require.NoError(t, h.runner.Run(context.Background(), h.project))

	entries, err := os.ReadDir(h.runner.cfg.StagingDir)
	if err != nil {
		require.True(t, os.IsNotExist(err))
		return
	}
	assert.Empty(t, entries, "no staging archives may be left behind")
}

func TestRunCleansUpStagingAfterFailure(t *testing.T) {
	h := newHarness(t, "db-data")
	h.store.failOnKey = "db-data.tar.gz"

	require.Error(t, h.runner.Run(context.Background(), h.project))

	entries, err := os.ReadDir(h.runner.cfg.StagingDir)
	if err != nil {
		require.True(t, os.IsNotExist(err))
		return
	}
	assert.Empty(t, entries)
}

func TestClassifyExit(t *testing.T) {
	ctx := context.Background()

	assert.Equal(t, ExitSuccess, classifyExit(ctx, nil))
	assert.Equal(t, ExitFailure, classifyExit(ctx, errors.New("boom")))
	assert.Equal(t, ExitTimeout, classifyExit(ctx, context.DeadlineExceeded))
	assert.Equal(t, ExitCancelled, classifyExit(ctx, context.Canceled))

	// A wrapped cause is still classified correctly.
	assert.Equal(t, ExitTimeout, classifyExit(ctx, fmt.Errorf("upload: %w", context.DeadlineExceeded)))

	// A cancelled context classifies even when the returned error is generic,
	// since the cancellation is what actually ended the run.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, ExitCancelled, classifyExit(cancelled, errors.New("write failed")))
}

func TestManagedVolumesFiltersEphemeral(t *testing.T) {
	spec := v1.ProjectSpec{Volumes: []v1.VolumeDef{
		{Name: "db-data", Type: v1.VolumeTypeManaged},
		{Name: "cache", Type: v1.VolumeTypeEphemeral},
		{Name: "uploads", Type: v1.VolumeTypeManaged},
	}}

	got := managedVolumes(spec)

	require.Len(t, got, 2)
	assert.Equal(t, "db-data", got[0].Name)
	assert.Equal(t, "uploads", got[1].Name)
}
