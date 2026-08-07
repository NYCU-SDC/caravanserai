package restore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStagingPresent(t *testing.T) {
	root := t.TempDir()

	got, err := StagingPresent(root, "default", "blog")
	require.NoError(t, err)
	assert.False(t, got, "a clean node has no staging")

	staging, err := StagingDir(root, "default", "blog")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	got, err = StagingPresent(root, "default", "blog")
	require.NoError(t, err)
	assert.True(t, got, "surviving staging means the previous restore never finished")
}

func TestStagingPresentIsPerProject(t *testing.T) {
	// One Project's interrupted restore must not force another to restore.
	root := t.TempDir()
	staging, err := StagingDir(root, "default", "blog")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	got, err := StagingPresent(root, "default", "wiki")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestCleanDisplacedLeavesStagingAlone(t *testing.T) {
	// Staging is not garbage: it is the signal that the previous restore did
	// not finish. Sweeping it here would erase that and let a half-swapped
	// Project be adopted on the next pass.
	root := t.TempDir()
	staging, err := StagingDir(root, "default", "blog")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "partial.tar.gz"), []byte("x"), 0o600))

	require.NoError(t, CleanDisplaced(root))

	present, err := StagingPresent(root, "default", "blog")
	require.NoError(t, err)
	assert.True(t, present, "the interrupted-restore signal must survive the sweep")
}

func TestCleanDisplacedRemovesDisplacedDirectories(t *testing.T) {
	// A crash between setting the previous data aside and swapping the new
	// data in leaves this behind. Nothing references it, and it occupies a
	// full copy of the volume.
	root := t.TempDir()
	displaced := filepath.Join(root, "volumes", "default", "blog", "db-data", "data"+displacedSuffix)
	require.NoError(t, os.MkdirAll(displaced, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(displaced, "old.txt"), []byte("old"), 0o600))

	require.NoError(t, CleanDisplaced(root))

	_, statErr := os.Stat(displaced)
	assert.True(t, os.IsNotExist(statErr))
}

func TestCleanDisplacedKeepsLiveVolumeData(t *testing.T) {
	// The sweep must be surgical: live volume data sits right next to the
	// displaced copy it removes.
	root := t.TempDir()

	live := filepath.Join(root, "volumes", "default", "blog", "db-data", "data")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "keep.txt"), []byte("live"), 0o600))

	displaced := live + displacedSuffix
	require.NoError(t, os.MkdirAll(displaced, 0o700))

	require.NoError(t, CleanDisplaced(root))

	got, err := os.ReadFile(filepath.Join(live, "keep.txt"))
	require.NoError(t, err, "live volume data must survive the sweep")
	assert.Equal(t, "live", string(got))

	_, statErr := os.Stat(displaced)
	assert.True(t, os.IsNotExist(statErr))
}

func TestCleanDisplacedOnEmptyDataRoot(t *testing.T) {
	// A node that has never run a restore has neither tree; that is not an
	// error.
	assert.NoError(t, CleanDisplaced(t.TempDir()))
}

func TestCleanDisplacedRejectsInvalidDataRoot(t *testing.T) {
	assert.Error(t, CleanDisplaced("relative/path"))
}
