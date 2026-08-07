package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackupID(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)

	id, err := NewBackupID(now)
	require.NoError(t, err)

	assert.Regexp(t, `^20260724T140000Z-[0-9a-f]{8}$`, id)

	// Two IDs generated at the same instant must not collide.
	id2, err := NewBackupID(now)
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
}

func TestBuildManifest(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	volumes := []VolumeManifestEntry{
		{Name: "db-data", ArchiveKey: "cara/v1/projects/default/blog/snapshots/x/db-data.tar.gz", SizeBytes: 100, SHA256: "abc"},
		{Name: "uploads", ArchiveKey: "cara/v1/projects/default/blog/snapshots/x/uploads.tar.gz", SizeBytes: 200, SHA256: "def"},
	}

	m := BuildManifest("20260724T140000Z-6e27c7b2", "default", "blog", "node-a", "1.0.0", 12, now, volumes)

	assert.Equal(t, 1, m.FormatVersion)
	assert.Equal(t, "20260724T140000Z-6e27c7b2", m.BackupID)
	assert.Equal(t, "default", m.Namespace)
	assert.Equal(t, "blog", m.Project)
	assert.Equal(t, "node-a", m.SourceNode)
	assert.Equal(t, int64(12), m.ProjectResourceVersion)
	assert.Equal(t, "tar-offline", m.BackupMethod)
	assert.Equal(t, "1.0.0", m.CaraVersion)
	assert.Equal(t, now, m.CreatedAt)
	assert.Equal(t, volumes, m.Volumes)
}

func TestManifestRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	original := BuildManifest("id-1", "default", "blog", "node-a", "1.0.0", 3, now, []VolumeManifestEntry{
		{Name: "db-data", ArchiveKey: "k", SizeBytes: 1, SHA256: "h"},
	})

	data, err := original.Marshal()
	require.NoError(t, err)

	got, err := UnmarshalManifest(data)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestUnmarshalManifestRejectsGarbage(t *testing.T) {
	_, err := UnmarshalManifest([]byte("not json"))
	assert.Error(t, err)
}

func TestLatestRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	original := NewLatest("id-1", "cara/v1/projects/default/blog/snapshots/id-1/manifest.json", now)

	assert.Equal(t, 1, original.SchemaVersion)

	data, err := original.Marshal()
	require.NoError(t, err)

	got, err := UnmarshalLatest(data)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestUnmarshalLatestRejectsGarbage(t *testing.T) {
	_, err := UnmarshalLatest([]byte("not json"))
	assert.Error(t, err)
}
