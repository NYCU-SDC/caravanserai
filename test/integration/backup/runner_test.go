//go:build e2e

package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	agentbackup "NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── minimal collaborators ────────────────────────────────────────────────────

type recordingContainers struct{ calls []string }

func (r *recordingContainers) StopProject(context.Context, *v1.Project) error {
	r.calls = append(r.calls, "stop")
	return nil
}

func (r *recordingContainers) StartProject(context.Context, *v1.Project) error {
	r.calls = append(r.calls, "start")
	return nil
}

type retainedOwnership struct{}

func (retainedOwnership) Resolve(context.Context, agentbackup.ResourceKey) agentbackup.Ownership {
	return agentbackup.OwnershipRetained
}

type noopConditions struct{}

func (noopConditions) SetMaintenance(context.Context, agentbackup.ResourceKey, string) error {
	return nil
}
func (noopConditions) ClearMaintenance(context.Context, agentbackup.ResourceKey) error { return nil }

// ── harness ──────────────────────────────────────────────────────────────────

type runnerHarness struct {
	runner     *agentbackup.Runner
	store      objectstore.ObjectStore
	containers *recordingContainers
	project    *v1.Project
	dataRoot   string
}

// newRunnerHarness builds a Runner writing to the real MinIO container, with
// each named volume backed by a real directory holding a marker file.
func newRunnerHarness(t *testing.T, projectName string, volumes ...string) *runnerHarness {
	t.Helper()

	dataRoot := t.TempDir()
	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName, Namespace: "default", ResourceVersion: 5},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: "node-a"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
			Backup:   &v1.ProjectBackupConfig{Interval: "168h"},
		},
	}

	for _, name := range volumes {
		project.Spec.Volumes = append(project.Spec.Volumes,
			v1.VolumeDef{Name: name, Type: v1.VolumeTypeManaged})
		dir := filepath.Join(dataRoot, "volumes", "default", projectName, name, "data")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "marker.txt"), []byte("contents of "+name), 0o600))
	}

	containers := &recordingContainers{}
	store := newStore(t)

	runner := agentbackup.NewRunner(
		agentbackup.NewCoordinator(), containers, store,
		retainedOwnership{}, noopConditions{}, nil,
		agentbackup.Config{DataRoot: dataRoot, NodeName: "node-a", CaraVersion: "e2e"},
		zap.NewNop(),
	)

	return &runnerHarness{
		runner: runner, store: store, containers: containers,
		project: project, dataRoot: dataRoot,
	}
}

func readJSON(t *testing.T, store objectstore.ObjectStore, key string) []byte {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	return data
}

// ── tests ────────────────────────────────────────────────────────────────────

// TestBackupWritesOneGenerationToMinio is the end-to-end shape of a backup:
// every Managed volume captured under one backupID, a manifest describing
// them, and only then the pointer a restore reads.
func TestBackupWritesOneGenerationToMinio(t *testing.T) {
	h := newRunnerHarness(t, "e2e-generation", "db-data", "uploads")
	ctx := context.Background()

	require.NoError(t, h.runner.Run(ctx, h.project))

	assert.Equal(t, []string{"stop", "start"}, h.containers.calls,
		"the project must be quiesced for the archive and brought back after")

	latestKey, err := agentbackup.LatestKey("default", "e2e-generation")
	require.NoError(t, err)
	latest, err := agentbackup.UnmarshalLatest(readJSON(t, h.store, latestKey))
	require.NoError(t, err)
	assert.NotEmpty(t, latest.BackupID)

	// The pointer must reference a manifest that exists.
	manifest, err := agentbackup.UnmarshalManifest(readJSON(t, h.store, latest.ManifestKey))
	require.NoError(t, err)
	assert.Equal(t, latest.BackupID, manifest.BackupID)
	assert.Equal(t, "node-a", manifest.SourceNode)
	assert.Equal(t, int64(5), manifest.ProjectResourceVersion)
	assert.Equal(t, "tar-offline", manifest.BackupMethod)
	require.Len(t, manifest.Volumes, 2, "both Managed volumes belong to this generation")

	// Every archive the manifest names must exist at the recorded size, and
	// live under the same generation prefix.
	for _, vol := range manifest.Volumes {
		meta, headErr := h.store.Head(ctx, vol.ArchiveKey)
		require.NoError(t, headErr, "manifest references a missing archive: %s", vol.ArchiveKey)
		assert.Equal(t, vol.SizeBytes, meta.Size,
			"stored size must match what the manifest recorded for %s", vol.Name)
		assert.Len(t, vol.SHA256, 64)
		assert.Contains(t, vol.ArchiveKey, "/snapshots/"+manifest.BackupID+"/")
	}
}

// TestBackupLeavesNoLatestPointerWhenAVolumeFails is the cross-volume
// atomicity guarantee. If any volume cannot be captured, the whole generation
// is abandoned: a restore must never pair one volume's new archive with
// another's old one.
func TestBackupLeavesNoLatestPointerWhenAVolumeFails(t *testing.T) {
	h := newRunnerHarness(t, "e2e-atomicity", "db-data", "uploads")
	ctx := context.Background()

	// Remove one volume's directory so archiving it fails partway through the
	// run — the closest analogue to an interrupted upload we can force
	// deterministically against a real server.
	require.NoError(t, os.RemoveAll(
		filepath.Join(h.dataRoot, "volumes", "default", "e2e-atomicity", "uploads")))

	err := h.runner.Run(ctx, h.project)
	require.Error(t, err)

	latestKey, keyErr := agentbackup.LatestKey("default", "e2e-atomicity")
	require.NoError(t, keyErr)
	_, headErr := h.store.Head(ctx, latestKey)
	assert.ErrorIs(t, headErr, objectstore.ErrNotFound,
		"latest.json must not exist when the generation was abandoned")

	// No manifest may be written for an incomplete generation either.
	prefix, prefixErr := agentbackup.ProjectPrefix("default", "e2e-atomicity")
	require.NoError(t, prefixErr)
	objects, listErr := h.store.List(ctx, prefix)
	require.NoError(t, listErr)
	for _, obj := range objects {
		assert.NotContains(t, obj.Key, "manifest.json",
			"an incomplete generation must not be described by a manifest")
	}

	assert.Equal(t, []string{"stop", "start"}, h.containers.calls,
		"a failed backup must still bring the service back")
}

// TestBackupDoesNotAdvanceLatestOnFailure is the guarantee that protects an
// existing restore point: a later run that fails must leave the previous
// generation current.
func TestBackupDoesNotAdvanceLatestOnFailure(t *testing.T) {
	h := newRunnerHarness(t, "e2e-no-regress", "db-data")
	ctx := context.Background()

	// First run succeeds and establishes a restore point.
	require.NoError(t, h.runner.Run(ctx, h.project))

	latestKey, err := agentbackup.LatestKey("default", "e2e-no-regress")
	require.NoError(t, err)
	firstLatest, err := agentbackup.UnmarshalLatest(readJSON(t, h.store, latestKey))
	require.NoError(t, err)

	// Second run fails after the pointer already exists.
	require.NoError(t, os.RemoveAll(
		filepath.Join(h.dataRoot, "volumes", "default", "e2e-no-regress", "db-data")))
	require.Error(t, h.runner.Run(ctx, h.project))

	secondLatest, err := agentbackup.UnmarshalLatest(readJSON(t, h.store, latestKey))
	require.NoError(t, err)
	assert.Equal(t, firstLatest.BackupID, secondLatest.BackupID,
		"a failed run must leave the previous generation current")

	// And the generation it points at is still fully intact.
	manifest, err := agentbackup.UnmarshalManifest(readJSON(t, h.store, secondLatest.ManifestKey))
	require.NoError(t, err)
	for _, vol := range manifest.Volumes {
		_, headErr := h.store.Head(ctx, vol.ArchiveKey)
		assert.NoError(t, headErr, "the surviving restore point must remain complete")
	}
}

// TestBackupSucceedsWithoutObjectStoreIsRefused checks the fail-closed rule:
// a Project asking to be backed up on an agent with no store must not run
// silently unbacked, and must not pay downtime to find out.
func TestBackupRefusedWithoutObjectStore(t *testing.T) {
	h := newRunnerHarness(t, "e2e-fail-closed", "db-data")

	runner := agentbackup.NewRunner(
		agentbackup.NewCoordinator(), h.containers, nil,
		retainedOwnership{}, noopConditions{}, nil,
		agentbackup.Config{DataRoot: h.dataRoot, NodeName: "node-a", CaraVersion: "e2e"},
		zap.NewNop(),
	)

	err := runner.Run(context.Background(), h.project)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no object store configured")
	assert.Empty(t, h.containers.calls, "the containers must never be stopped")
}

// TestBackupArchiveContainsVolumeData confirms the archives hold the real
// bytes, not just plausible metadata.
func TestBackupArchiveContainsVolumeData(t *testing.T) {
	h := newRunnerHarness(t, "e2e-contents", "db-data")
	ctx := context.Background()

	require.NoError(t, h.runner.Run(ctx, h.project))

	latestKey, err := agentbackup.LatestKey("default", "e2e-contents")
	require.NoError(t, err)
	latest, err := agentbackup.UnmarshalLatest(readJSON(t, h.store, latestKey))
	require.NoError(t, err)
	manifest, err := agentbackup.UnmarshalManifest(readJSON(t, h.store, latest.ManifestKey))
	require.NoError(t, err)
	require.Len(t, manifest.Volumes, 1)

	rc, _, err := h.store.Get(ctx, manifest.Volumes[0].ArchiveKey)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	names, contents := readTarGz(t, rc)
	assert.Contains(t, names, "marker.txt")
	assert.Contains(t, strings.Join(contents, "\n"), "contents of db-data")
}
