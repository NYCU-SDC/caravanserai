package restore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkerPath(t *testing.T) {
	got, err := MarkerPath("/var/lib/cara", "default", "blog")
	require.NoError(t, err)

	assert.Equal(t, "/var/lib/cara/volumes/default/blog/.cara-restore.json", got)

	// The marker must sit beside the volume directories, never inside one:
	// an atomic swap replaces {volume}/data wholesale, and a backup archives
	// exactly that directory.
	assert.NotContains(t, got, "/data/")
}

func TestMarkerPathRejectsInvalidNames(t *testing.T) {
	_, err := MarkerPath("/var/lib/cara", "default", "../escape")
	assert.Error(t, err)
}

func TestReadMarkerAbsent(t *testing.T) {
	// Absent is the normal state for a fresh placement and must not be an
	// error — the caller distinguishes it by the nil marker.
	m, err := ReadMarker(t.TempDir(), "default", "blog")
	require.NoError(t, err)
	assert.Nil(t, m)
}

func TestWriteThenReadMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	require.NoError(t, WriteMarker(root, "default", "blog", "20260730T120000Z-abcd1234", now))

	m, err := ReadMarker(root, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)

	assert.Equal(t, "default", m.Namespace)
	assert.Equal(t, "blog", m.Project)
	assert.Equal(t, "20260730T120000Z-abcd1234", m.BackupID)
	assert.Equal(t, now, m.RestoredAt)
}

func TestWriteMarkerWithoutBackupID(t *testing.T) {
	// Volumes initialised empty because no generation existed yet still get a
	// marker — its presence is what stops a later restore from overwriting
	// whatever the containers have written since.
	root := t.TempDir()

	require.NoError(t, WriteMarker(root, "default", "blog", "", time.Now()))

	m, err := ReadMarker(root, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Empty(t, m.BackupID)
}

func TestWriteMarkerIsAtomic(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, WriteMarker(root, "default", "blog", "gen-1", time.Now()))

	path, err := MarkerPath(root, "default", "blog")
	require.NoError(t, err)

	// No temp file may survive a successful write.
	_, err = os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "the temp file must be renamed away, not left behind")
}

func TestWriteMarkerOverwrites(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, WriteMarker(root, "default", "blog", "gen-1", time.Now()))
	require.NoError(t, WriteMarker(root, "default", "blog", "gen-2", time.Now()))

	m, err := ReadMarker(root, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, "gen-2", m.BackupID)
}

func TestReadMarkerRejectsCorruptFile(t *testing.T) {
	root := t.TempDir()
	path, err := MarkerPath(root, "default", "blog")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))

	_, err = ReadMarker(root, "default", "blog")
	assert.ErrorContains(t, err, "parse marker")
}

func TestMarkerSurvivesVolumeDirectoryReplacement(t *testing.T) {
	// The reason the marker lives at Project level: restore swaps each
	// {volume}/data directory wholesale, and the marker must outlive that.
	root := t.TempDir()
	require.NoError(t, WriteMarker(root, "default", "blog", "gen-1", time.Now()))

	volumeData := filepath.Join(root, "volumes", "default", "blog", "db-data", "data")
	require.NoError(t, os.MkdirAll(volumeData, 0o700))
	require.NoError(t, os.RemoveAll(volumeData))
	require.NoError(t, os.MkdirAll(volumeData, 0o700))

	m, err := ReadMarker(root, "default", "blog")
	require.NoError(t, err)
	assert.NotNil(t, m, "replacing a volume directory must not remove the marker")
}
