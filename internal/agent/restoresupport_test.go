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
	err := ensureVolumeData(context.Background(), nil, backup.NewCoordinator(), t.TempDir(), testNode,
		testProject(managedVolume("db-data")), zap.NewNop())
	assert.NoError(t, err)
}

func TestEnsureVolumeDataIgnoresProjectsWithoutManagedVolumes(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})

	p := testProject(v1.VolumeDef{Name: "cache", Type: v1.VolumeTypeEphemeral})
	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
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

	assert.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
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

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
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

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
		p, zap.NewNop())

	var blocked *restore.PlacementBlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, restore.BlockForeignProvenance, blocked.Reason)
	assert.Contains(t, blocked.Detail, "assignmentGeneration")
}

func TestEnsureVolumeDataInitialisesEmptyOnlyForANeverAssignedProject(t *testing.T) {
	// A Project that has provably never been placed anywhere has no data to
	// recover, so starting empty is the correct state rather than a guess.
	// AssignmentHistory is what proves it; "the object store has no pointer"
	// does not, and that conflation is the defect below.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))
	p.Status.AssignmentHistory = v1.AssignmentHistoryNeverAssigned

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
		p, zap.NewNop()))

	live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "db-data", "data")
	info, err := os.Stat(live)
	require.NoError(t, err, "an empty start still needs the volume directory to mount")
	assert.True(t, info.IsDir())

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NotNil(t, marker, "an empty start is still this node establishing its data")
	assert.Equal(t, restore.MarkerVersion, marker.Version)
	assert.Equal(t, "uid-1", marker.ProjectUID)
	assert.Equal(t, testNode, marker.NodeName)
	assert.EqualValues(t, 7, marker.AssignmentGeneration)
	assert.Empty(t, marker.InitializedFromBackupID, "nothing was restored")
}

// The defect: a missing latest.json used to mean "never backed up", and the
// response was to start empty. For a Project that has been placed before, the
// same signal means a bucket typo, an object-store outage, or a backup that
// never completed — and starting empty turns any of those into a successful
// deployment whose emptiness the Backup Supervisor then archives over the
// generation that held the real data.
func TestEnsureVolumeDataBlocksWhenAPlacedProjectHasNoBackup(t *testing.T) {
	for _, history := range []v1.AssignmentHistory{
		v1.AssignmentHistoryKnown,
		v1.AssignmentHistoryUnknown,
	} {
		t.Run(string(history), func(t *testing.T) {
			restorer, coordinator, dataRoot := newSupport(t, missingStore{})
			p := testProject(managedVolume("db-data"))
			p.Status.AssignmentHistory = history

			err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
				p, zap.NewNop())

			var blocked *restore.PlacementBlockedError
			require.ErrorAs(t, err, &blocked)
			assert.Equal(t, restore.BlockNoRestoreSource, blocked.Reason)

			live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "db-data", "data")
			_, statErr := os.Stat(live)
			assert.True(t, os.IsNotExist(statErr),
				"a block must not leave a directory a container could mount as success")

			marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
			require.NoError(t, err)
			assert.Nil(t, marker, "nothing was established, so nothing is claimed")
		})
	}
}

// Every refusal reaches an operator through Project status, so the message may
// name what the Project's own spec names and must not publish this node's
// filesystem layout.
func TestPlacementBlockDetailsNameNoHostPaths(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "unproven")

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
		p, zap.NewNop())

	var blocked *restore.PlacementBlockedError
	require.ErrorAs(t, err, &blocked)
	assert.NotContains(t, blocked.Detail, dataRoot)
	assert.NotContains(t, blocked.Detail, "/volumes/")
}

func TestEnsureVolumeDataDefersWhenProjectIsBusy(t *testing.T) {
	// A backup holds the Project. Restoring underneath it would move the same
	// bytes the backup is reading, so the tick yields rather than fails.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))

	release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpBackup)
	require.True(t, ok)
	defer release()

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
		p, zap.NewNop())
	assert.ErrorIs(t, err, errDeferred, "a lost race is a retry, not a failure")
}

func TestEnsureVolumeDataReleasesClaimOnReturn(t *testing.T) {
	// The claim must not outlive the call, or the poll loop would skip this
	// Project forever. A refusal is the path that matters: it returns early,
	// and an early return is where a defer is easiest to lose.
	key := backup.ResourceKey{Namespace: "default", Name: "blog"}

	t.Run("after a successful empty initialisation", func(t *testing.T) {
		restorer, coordinator, dataRoot := newSupport(t, missingStore{})
		p := testProject(managedVolume("db-data"))
		p.Status.AssignmentHistory = v1.AssignmentHistoryNeverAssigned

		require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
			p, zap.NewNop()))

		assert.False(t, coordinator.IsBusy(key))
	})

	t.Run("after a refusal", func(t *testing.T) {
		restorer, coordinator, dataRoot := newSupport(t, missingStore{})
		p := testProject(managedVolume("db-data"))

		require.Error(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
			p, zap.NewNop()))

		assert.False(t, coordinator.IsBusy(key))
	})
}

func TestStagingOutranksAMatchingMarkerByPolicy(t *testing.T) {
	// Leftover staging beats a marker that names this exact assignment. That
	// is a deliberate fail-closed choice, not a claim that staging proves the
	// restore was interrupted — the swap may have completed and only the
	// cleanup failed, in which case restoring again costs recent writes.
	//
	// It is preferred anyway because the case it guards is the one no marker
	// can warn about: a swap that failed and whose rollback also failed leaves
	// volumes split across two generations, with the *previous* marker still
	// on disk reading as authoritative. Serving that is worse than redoing a
	// restore.
	_, _, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "looks complete")
	require.NoError(t, restore.WriteMarker(dataRoot, currentProvenance(p, "20260801T000000Z"), nowUTC()))

	staging, err := restore.StagingDir(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	outcome, err := decideRestore(dataRoot, currentProvenance(p, ""), p)
	require.NoError(t, err)
	assert.Equal(t, restore.DecisionRestore, outcome.Decision,
		"a matching marker must not short-circuit leftover staging")
}

func TestHasManagedVolume(t *testing.T) {
	assert.False(t, hasManagedVolume(nil))
	assert.False(t, hasManagedVolume([]v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}))
	assert.True(t, hasManagedVolume([]v1.VolumeDef{
		{Name: "cache", Type: v1.VolumeTypeEphemeral},
		managedVolume("db-data"),
	}))
}

// Shadow mode must not be weaker than what it replaces. Every other block
// reason downgrades to what the previous release did, but an unreadable marker
// already stopped the placement there — ReadMarker returned an error and
// nothing started. Letting the compatibility default turn that into "start on
// data of unknown origin" would make this change reduce an existing
// protection under its own default.
func TestEnsureVolumeDataCorruptMarkerBlocksEvenInShadowMode(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	writeLive(t, dataRoot, p, "db-data", "unknown origin")

	path, err := restore.MarkerPath(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	for _, strict := range []bool{false, true} {
		err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
			p, zap.NewNop())

		var blocked *restore.PlacementBlockedError
		require.ErrorAs(t, err, &blocked, "strict=%v", strict)
		assert.Equal(t, restore.BlockCorruptProvenance, blocked.Reason)
	}
}

// The partial-loss case at the agent level: one volume healthy, one gone. The
// marker matches this assignment, so the old single-boolean check said the
// data was present and skipped the restore.
func TestEnsureVolumeDataBlocksWhenOneVolumeIsMissing(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"), managedVolume("uploads"))
	writeLive(t, dataRoot, p, "db-data", "still here")
	// uploads is never created: the directory is gone.

	require.NoError(t, restore.WriteMarker(dataRoot, currentProvenance(p, "20260801T000000Z"), nowUTC()))

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, testNode,
		p, zap.NewNop())

	var blocked *restore.PlacementBlockedError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, restore.BlockMissingData, blocked.Reason)
	assert.Contains(t, blocked.Detail, "uploads")
}
