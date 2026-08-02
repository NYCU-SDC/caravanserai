package restore

import (
	"os"
	"path/filepath"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name            string
		stagingPresent  bool
		markerPresent   bool
		volumesHaveData bool
		want            Decision
		why             string
	}{
		{
			name:            "no marker and no data is a fresh placement",
			volumesHaveData: false,
			want:            DecisionRestore,
			why:             "nothing local to lose, so restoring is safe",
		},
		{
			name:            "marker present means this node already served the project",
			markerPresent:   true,
			volumesHaveData: true,
			want:            DecisionSkip,
			why:             "local volumes may have moved ahead of the newest generation",
		},
		{
			name:            "marker present with empty volumes still skips",
			markerPresent:   true,
			volumesHaveData: false,
			want:            DecisionSkip,
			why: "a legitimately empty volume must not be repopulated from an older " +
				"generation — that would undo a deliberate deletion",
		},
		{
			name:            "data without a marker is adopted, never overwritten",
			markerPresent:   false,
			volumesHaveData: true,
			want:            DecisionAdoptExisting,
			why: "a project born on this node, or one predating markers, has live " +
				"data that a restore would destroy",
		},
		{
			name:            "leftover staging outranks a marker",
			stagingPresent:  true,
			markerPresent:   true,
			volumesHaveData: true,
			want:            DecisionRestore,
			why: "the previous restore died mid-swap, so the marker and the volumes " +
				"may describe different generations",
		},
		{
			name:            "leftover staging outranks existing data",
			stagingPresent:  true,
			markerPresent:   false,
			volumesHaveData: true,
			want:            DecisionRestore,
			why: "adopting here would freeze a half-swapped volume set and bless it " +
				"with a marker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.stagingPresent, tt.markerPresent, tt.volumesHaveData)
			assert.Equal(t, tt.want, got, tt.why)
		})
	}
}

func TestDecideStagingAlwaysForcesRestore(t *testing.T) {
	// Whatever else is on disk, an interrupted restore must never be adopted:
	// its volumes may be split across generations.
	for _, marker := range []bool{true, false} {
		for _, data := range []bool{true, false} {
			assert.Equal(t, DecisionRestore, Decide(true, marker, data),
				"staging=true marker=%v data=%v must restore", marker, data)
		}
	}
}

func TestDecideNeverRestoresOverLocalData(t *testing.T) {
	// The single property this package exists to guarantee: if there is any
	// local data, no combination of inputs may produce a restore.
	for _, markerPresent := range []bool{true, false} {
		assert.NotEqual(t, DecisionRestore, Decide(false, markerPresent, true),
			"markerPresent=%v with data on disk must never restore", markerPresent)
	}
}

func TestDecisionString(t *testing.T) {
	assert.Equal(t, "Restore", DecisionRestore.String())
	assert.Equal(t, "Skip", DecisionSkip.String())
	assert.Equal(t, "AdoptExisting", DecisionAdoptExisting.String())
}

// writeVolumeFile creates a Managed volume's data directory and optionally
// puts a file in it.
func writeVolumeFile(t *testing.T, dataRoot, namespace, project, volume string, withContent bool) {
	t.Helper()
	dir := filepath.Join(dataRoot, "volumes", namespace, project, volume, "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	if withContent {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "content.txt"), []byte("x"), 0o600))
	}
}

func TestVolumesHaveData(t *testing.T) {
	managed := []v1.VolumeDef{
		{Name: "db-data", Type: v1.VolumeTypeManaged},
		{Name: "uploads", Type: v1.VolumeTypeManaged},
	}

	t.Run("absent directories count as no data", func(t *testing.T) {
		root := t.TempDir()
		got, err := VolumesHaveData(root, "default", "blog", managed)
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("existing but empty directories count as no data", func(t *testing.T) {
		// The agent provisions empty directories for Managed volumes, so mere
		// existence must not be read as "this node has served the project".
		root := t.TempDir()
		writeVolumeFile(t, root, "default", "blog", "db-data", false)
		writeVolumeFile(t, root, "default", "blog", "uploads", false)

		got, err := VolumesHaveData(root, "default", "blog", managed)
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("content in any single volume counts", func(t *testing.T) {
		root := t.TempDir()
		writeVolumeFile(t, root, "default", "blog", "db-data", false)
		writeVolumeFile(t, root, "default", "blog", "uploads", true)

		got, err := VolumesHaveData(root, "default", "blog", managed)
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("ephemeral volumes are ignored", func(t *testing.T) {
		root := t.TempDir()
		writeVolumeFile(t, root, "default", "blog", "cache", true)

		got, err := VolumesHaveData(root, "default", "blog",
			[]v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}})
		require.NoError(t, err)
		assert.False(t, got, "Ephemeral volumes are never restored, so their content is irrelevant")
	})

	t.Run("invalid volume name is rejected", func(t *testing.T) {
		_, err := VolumesHaveData(t.TempDir(), "default", "blog",
			[]v1.VolumeDef{{Name: "../escape", Type: v1.VolumeTypeManaged}})
		assert.Error(t, err)
	})
}

func TestStagingDir(t *testing.T) {
	got, err := StagingDir("/var/lib/cara", "default", "blog")
	require.NoError(t, err)

	assert.Equal(t, "/var/lib/cara/restore-staging/default/blog", got)
	assert.NotContains(t, got, "/volumes/",
		"staging must live outside the volumes tree so a partial extraction "+
			"can never be mistaken for live volume data")
}

func TestStagingDirRejectsInvalidNames(t *testing.T) {
	_, err := StagingDir("/var/lib/cara", "default", "../escape")
	assert.Error(t, err)
}
