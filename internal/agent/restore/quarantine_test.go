package restore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var quarantineOwner = Provenance{
	Namespace: "default", Project: "blog", ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 3,
}

var quarantineNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// placeData writes a volume file and, when marker is given, its marker.
func placeData(t *testing.T, dataRoot string, marker *Provenance) string {
	t.Helper()
	dir := filepath.Join(dataRoot, "volumes", "default", "blog")
	data := filepath.Join(dir, "db-data", "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(data, "state.txt"), []byte("hello"), 0o600))
	if marker != nil {
		require.NoError(t, WriteMarker(dataRoot, *marker, quarantineNow))
	}
	return dir
}

func quarantinedCopy(dataRoot string) string {
	return filepath.Join(dataRoot, "quarantine", "default", "blog", "uid-1-gen3-20261031T120000Z")
}

// What matters afterwards is that the next placement sees no local data and a
// restore is what follows, while the bytes still exist.
func TestQuarantineMovesTheAssignmentsData(t *testing.T) {
	dataRoot := t.TempDir()
	dir := placeData(t, dataRoot, &quarantineOwner)

	require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow))

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the active path must be empty")
	marker, err := ReadMarker(dataRoot, "default", "blog")
	require.NoError(t, err)
	assert.Nil(t, marker, "no marker may remain at the active path")

	got, err := os.ReadFile(filepath.Join(quarantinedCopy(dataRoot), "db-data", "data", "state.txt"))
	require.NoError(t, err, "quarantine must not lose the data")
	assert.Equal(t, "hello", string(got))
	_, err = os.Stat(filepath.Join(quarantinedCopy(dataRoot), markerFilename))
	assert.NoError(t, err, "the marker travels with the data")
}

func TestQuarantineIsIdempotent(t *testing.T) {
	dataRoot := t.TempDir()
	placeData(t, dataRoot, &quarantineOwner)

	require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow))
	require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow.Add(time.Minute)))

	entries, err := os.ReadDir(filepath.Dir(quarantinedCopy(dataRoot)))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the second call must find nothing to move")
}

func TestQuarantineWithNothingOnDiskIsNotAnError(t *testing.T) {
	assert.NoError(t, Quarantine(t.TempDir(), quarantineOwner, quarantineNow))
}

// Another generation's or lifetime's data is not this obligation's to move.
func TestQuarantineLeavesAnotherAssignmentsData(t *testing.T) {
	for name, change := range map[string]func(*Provenance){
		"later generation": func(p *Provenance) { p.AssignmentGeneration = 4 },
		"another lifetime": func(p *Provenance) { p.ProjectUID = "uid-2" },
		"another node":     func(p *Provenance) { p.NodeName = "node-b" },
	} {
		t.Run(name, func(t *testing.T) {
			dataRoot := t.TempDir()
			other := quarantineOwner
			change(&other)
			dir := placeData(t, dataRoot, &other)

			require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow))

			_, err := os.Stat(filepath.Join(dir, "db-data", "data", "state.txt"))
			assert.NoError(t, err)
			_, err = os.Stat(filepath.Join(dataRoot, "quarantine"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

// Data no marker accounts for would otherwise block every later placement here.
func TestQuarantineMovesDataWithNoProvableOwner(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		dataRoot := t.TempDir()
		dir := placeData(t, dataRoot, nil)

		require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow))

		_, err := os.Stat(dir)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("unreadable marker", func(t *testing.T) {
		dataRoot := t.TempDir()
		dir := placeData(t, dataRoot, nil)
		require.NoError(t, os.WriteFile(filepath.Join(dir, markerFilename), []byte("not json"), 0o600))

		require.NoError(t, Quarantine(dataRoot, quarantineOwner, quarantineNow))

		_, err := os.Stat(dir)
		assert.True(t, os.IsNotExist(err))
	})
}
