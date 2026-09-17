package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/restore"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// refusingStore fails every read. Any test using it asserts that the object
// store was never consulted — reaching it is the failure the test detects.
type refusingStore struct{ t *testing.T }

func (s refusingStore) Get(context.Context, string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	s.t.Helper()
	s.t.Error("object store must not be consulted on this path")
	return nil, objectstore.ObjectMeta{}, errors.New("refusingStore")
}

// missingStore reports every key as absent, which is how a Project that has
// never been backed up looks from the agent's side.
type missingStore struct{}

func (missingStore) Get(context.Context, string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	return nil, objectstore.ObjectMeta{}, objectstore.ErrNotFound
}

// recordedLogger returns a logger whose entries a test can inspect, for the
// cases where the log line is the deliverable rather than a side effect.
func recordedLogger() (*observer.ObservedLogs, *zap.Logger) {
	core, logs := observer.New(zapcore.DebugLevel)
	return logs, zap.New(core)
}

const testNode = "node-a"

func testProject(volumes ...v1.VolumeDef) *v1.Project {
	p := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: "uid-1"},
		Spec:       v1.ProjectSpec{Volumes: volumes},
	}
	p.Status.AssignmentGeneration = 7
	p.Status.AssignmentHistory = v1.AssignmentHistoryKnown
	return p
}

// currentProvenance is what this node would write for testProject.
func currentProvenance(p *v1.Project, backupID string) restore.Provenance {
	return restore.ProvenanceFor(p, testNode, backupID)
}

func managedVolume(name string) v1.VolumeDef {
	return v1.VolumeDef{Name: name, Type: v1.VolumeTypeManaged}
}

func newSupport(t *testing.T, store restore.Store) (*restore.Restorer, *backup.Coordinator, string) {
	t.Helper()
	dataRoot := t.TempDir()
	restorer := restore.NewRestorer(store, restore.Config{DataRoot: dataRoot, NodeName: testNode}, zap.NewNop())
	return restorer, backup.NewCoordinator(), dataRoot
}

func TestEnsureVolumeDataNoRestorerIsNotAnError(t *testing.T) {
	// An agent with no object store still runs Managed volumes; they simply
	// live and die on local disk.
	err := ensureVolumeData(context.Background(), nil, backup.NewCoordinator(), t.TempDir(), testNode, true,
		testProject(managedVolume("db-data")), zap.NewNop())
	assert.NoError(t, err)
}

func TestEnsureVolumeDataIgnoresProjectsWithoutManagedVolumes(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})

	p := testProject(v1.VolumeDef{Name: "cache", Type: v1.VolumeTypeEphemeral})
	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop()))

	// No marker either: a Project with nothing to restore should leave no
	// trace on disk.
	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	assert.Nil(t, marker)
}

// writeLive puts content into a Project's Managed volume directory and returns
// the path, so a test can assert afterwards that it was left alone.
func writeLive(t *testing.T, dataRoot string, p *v1.Project, volume, content string) string {
	t.Helper()
	live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, volume, "data")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "local.txt"), []byte(content), 0o600))
	return live
}

func TestEnsureVolumeDataSkipsWhenProvenanceMatches(t *testing.T) {
	// Provenance proves the data belongs to this exact assignment. Restoring
	// again would overwrite everything written since the last backup, which is
	// the failure mode the whole marker mechanism exists to prevent.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "mine")
	require.NoError(t, restore.WriteMarker(dataRoot, currentProvenance(p, "20260801T000000Z"), nowUTC()))

	assert.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop()))
}

func TestEnsureVolumeDataBlocksDataWithNoProvenance(t *testing.T) {
	// The defect: data with no marker used to be adopted and stamped as this
	// assignment's. A directory at the expected path proves nothing — it can
	// be what a previous lifetime, or a previous placement on this Node, left
	// behind.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	live := writeLive(t, dataRoot, p, "db-data", "someone else's")

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop())

	var blocked *restore.PlacementBlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, restore.BlockUnprovenData, blocked.Reason)

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	assert.Nil(t, marker, "a blocked placement must not stamp unproven data as its own")

	got, err := os.ReadFile(filepath.Join(live, "local.txt"))
	require.NoError(t, err, "blocking must not delete the bytes it refused to use")
	assert.Equal(t, "someone else's", string(got))
}

func TestEnsureVolumeDataBlocksAnotherAssignmentsData(t *testing.T) {
	// A→B→A: same Project, same Node, earlier generation. The data predates a
	// period when another Node owned the Project and may have moved it on.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "from generation 5")

	older := currentProvenance(p, "20260801T000000Z")
	older.AssignmentGeneration = 5
	require.NoError(t, restore.WriteMarker(dataRoot, older, nowUTC()))

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop())

	var blocked *restore.PlacementBlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, restore.BlockForeignProvenance, blocked.Reason)
	assert.Contains(t, blocked.Detail, "assignmentGeneration")
}

func TestEnsureVolumeDataShadowModeReportsButDoesNotBlock(t *testing.T) {
	// Shadow mode is how a deployment full of v1 markers is observed before
	// the rules start refusing placements. It must not change the outcome.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "unproven")

	logs, logger := recordedLogger()

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, false,
		p, logger), "shadow mode must not block")

	entry := logs.FilterMessageSnippet("would block this placement").All()
	require.Len(t, entry, 1)
	assert.Equal(t, zapcore.WarnLevel, entry[0].Level,
		"a judgement nobody can see is the defect this replaces")

	fields := entry[0].ContextMap()
	assert.Equal(t, string(restore.BlockUnprovenData), fields["reason"])
	assert.Equal(t, "uid-1", fields["projectUID"])
	assert.Equal(t, testNode, fields["nodeName"])
	assert.EqualValues(t, 7, fields["assignmentGeneration"])
}

func TestEnsureVolumeDataInitialisesEmptyWhenNeverBackedUp(t *testing.T) {
	// No generation has ever been written, so there is none to be missing and
	// starting empty is the correct state — not data loss.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop()))

	live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "db-data", "data")
	info, err := os.Stat(live)
	require.NoError(t, err, "an empty start still needs the volume directory to mount")
	assert.True(t, info.IsDir())

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	assert.NotNil(t, marker, "an empty start is still this node establishing its data")
}

func TestEnsureVolumeDataDefersWhenProjectIsBusy(t *testing.T) {
	// A backup holds the Project. Restoring underneath it would move the same
	// bytes the backup is reading, so the tick yields rather than fails.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))

	release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpBackup)
	require.True(t, ok)
	defer release()

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop())
	assert.ErrorIs(t, err, errDeferred, "a lost race is a retry, not a failure")
}

func TestEnsureVolumeDataReleasesClaimOnReturn(t *testing.T) {
	// The claim must not outlive the call, or the poll loop would skip this
	// Project forever.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop()))

	assert.False(t, coordinator.IsBusy(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}))
}

func TestEnsureVolumeDataRestoresWhenStagingSurvives(t *testing.T) {
	// Staging left on disk means the previous restore died mid-flight, so the
	// volumes may be split across generations. That must restore even though
	// data is present and a marker says this node owns it.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, restore.WriteMarker(dataRoot, currentProvenance(p, "20260801T000000Z"), nowUTC()))
	staging, err := restore.StagingDir(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	// missingStore makes the restore resolve to "never backed up", which the
	// caller treats as an empty start; the point here is only that the marker
	// did not short-circuit it. The rewritten marker is the evidence — a Skip
	// would have left the original generation ID in place.
	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode, true,
		p, zap.NewNop()))

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NotNil(t, marker)
	assert.Empty(t, marker.InitializedFromBackupID, "staging must defeat the marker and re-establish the data")
}

func TestHasManagedVolume(t *testing.T) {
	assert.False(t, hasManagedVolume(nil))
	assert.False(t, hasManagedVolume([]v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}))
	assert.True(t, hasManagedVolume([]v1.VolumeDef{
		{Name: "cache", Type: v1.VolumeTypeEphemeral},
		managedVolume("db-data"),
	}))
}
