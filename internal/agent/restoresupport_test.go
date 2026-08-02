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

func testProject(volumes ...v1.VolumeDef) *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default"},
		Spec:       v1.ProjectSpec{Volumes: volumes},
	}
}

func managedVolume(name string) v1.VolumeDef {
	return v1.VolumeDef{Name: name, Type: v1.VolumeTypeManaged}
}

func newSupport(t *testing.T, store restore.Store) (*restore.Restorer, *backup.Coordinator, string) {
	t.Helper()
	dataRoot := t.TempDir()
	restorer := restore.NewRestorer(store, restore.Config{DataRoot: dataRoot}, zap.NewNop())
	return restorer, backup.NewCoordinator(), dataRoot
}

func TestEnsureVolumeDataNoRestorerIsNotAnError(t *testing.T) {
	// An agent with no object store still runs Managed volumes; they simply
	// live and die on local disk.
	err := ensureVolumeData(context.Background(), nil, backup.NewCoordinator(), t.TempDir(),
		testProject(managedVolume("db-data")), zap.NewNop())
	assert.NoError(t, err)
}

func TestEnsureVolumeDataIgnoresProjectsWithoutManagedVolumes(t *testing.T) {
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})

	p := testProject(v1.VolumeDef{Name: "cache", Type: v1.VolumeTypeEphemeral})
	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))

	// No marker either: a Project with nothing to restore should leave no
	// trace on disk.
	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	assert.Nil(t, marker)
}

func TestEnsureVolumeDataSkipsWhenMarkerExists(t *testing.T) {
	// The marker says this node already established its data. Restoring again
	// would overwrite writes made since.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))
	require.NoError(t, restore.WriteMarker(dataRoot, p.Namespace, p.Name, "20260801T000000Z", nowUTC()))

	assert.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))
}

func TestEnsureVolumeDataAdoptsExistingData(t *testing.T) {
	// Data but no marker: a Project born on this node, or one predating
	// markers. Adopting records ownership without touching the bytes.
	restorer, coordinator, dataRoot := newSupport(t, refusingStore{t})
	p := testProject(managedVolume("db-data"))

	live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "db-data", "data")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "local.txt"), []byte("mine"), 0o600))

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))

	got, err := os.ReadFile(filepath.Join(live, "local.txt"))
	require.NoError(t, err, "adopting must not touch existing data")
	assert.Equal(t, "mine", string(got))

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NotNil(t, marker, "adoption must record ownership so later passes skip")
	assert.Empty(t, marker.BackupID, "adopted data came from no generation")
}

func TestEnsureVolumeDataInitialisesEmptyWhenNeverBackedUp(t *testing.T) {
	// No generation has ever been written, so there is none to be missing and
	// starting empty is the correct state — not data loss.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))

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

	err := ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop())
	assert.ErrorIs(t, err, errDeferred, "a lost race is a retry, not a failure")
}

func TestEnsureVolumeDataReleasesClaimOnReturn(t *testing.T) {
	// The claim must not outlive the call, or the poll loop would skip this
	// Project forever.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))

	assert.False(t, coordinator.IsBusy(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}))
}

func TestEnsureVolumeDataRestoresWhenStagingSurvives(t *testing.T) {
	// Staging left on disk means the previous restore died mid-flight, so the
	// volumes may be split across generations. That must restore even though
	// data is present and a marker says this node owns it.
	restorer, coordinator, dataRoot := newSupport(t, missingStore{})
	p := testProject(managedVolume("db-data"))

	require.NoError(t, restore.WriteMarker(dataRoot, p.Namespace, p.Name, "20260801T000000Z", nowUTC()))
	staging, err := restore.StagingDir(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	// missingStore makes the restore resolve to "never backed up", which the
	// caller treats as an empty start; the point here is only that the marker
	// did not short-circuit it. The rewritten marker is the evidence — a Skip
	// would have left the original generation ID in place.
	require.NoError(t, ensureVolumeData(context.Background(), restorer, coordinator, dataRoot, p, zap.NewNop()))

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	require.NoError(t, err)
	require.NotNil(t, marker)
	assert.Empty(t, marker.BackupID, "staging must defeat the marker and re-establish the data")
}

func TestHasManagedVolume(t *testing.T) {
	assert.False(t, hasManagedVolume(nil))
	assert.False(t, hasManagedVolume([]v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}))
	assert.True(t, hasManagedVolume([]v1.VolumeDef{
		{Name: "cache", Type: v1.VolumeTypeEphemeral},
		managedVolume("db-data"),
	}))
}
