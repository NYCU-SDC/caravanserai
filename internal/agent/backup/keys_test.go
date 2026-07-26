package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyLayout(t *testing.T) {
	const (
		ns       = "default"
		project  = "blog"
		backupID = "20260724T140000Z-6e27c7b2"
	)

	t.Run("ProjectPrefix", func(t *testing.T) {
		got, err := ProjectPrefix(ns, project)
		require.NoError(t, err)
		assert.Equal(t, "cara/v1/projects/default/blog", got)
	})

	t.Run("LatestKey", func(t *testing.T) {
		got, err := LatestKey(ns, project)
		require.NoError(t, err)
		assert.Equal(t, "cara/v1/projects/default/blog/latest.json", got)
	})

	t.Run("GenerationPrefix", func(t *testing.T) {
		got, err := GenerationPrefix(ns, project, backupID)
		require.NoError(t, err)
		assert.Equal(t, "cara/v1/projects/default/blog/snapshots/20260724T140000Z-6e27c7b2", got)
	})

	t.Run("ManifestKey", func(t *testing.T) {
		got, err := ManifestKey(ns, project, backupID)
		require.NoError(t, err)
		assert.Equal(t, "cara/v1/projects/default/blog/snapshots/20260724T140000Z-6e27c7b2/manifest.json", got)
	})

	t.Run("ArchiveKey", func(t *testing.T) {
		got, err := ArchiveKey(ns, project, backupID, "db-data")
		require.NoError(t, err)
		assert.Equal(t, "cara/v1/projects/default/blog/snapshots/20260724T140000Z-6e27c7b2/db-data.tar.gz", got)
	})

	t.Run("two volumes in one generation share the prefix", func(t *testing.T) {
		a, err := ArchiveKey(ns, project, backupID, "db-data")
		require.NoError(t, err)
		b, err := ArchiveKey(ns, project, backupID, "uploads")
		require.NoError(t, err)
		assert.NotEqual(t, a, b)
		assert.Contains(t, a, "/snapshots/"+backupID+"/")
		assert.Contains(t, b, "/snapshots/"+backupID+"/")
	})
}

func TestKeyLayoutRejectsInvalidNames(t *testing.T) {
	t.Run("invalid namespace", func(t *testing.T) {
		_, err := ProjectPrefix("../etc", "blog")
		assert.Error(t, err)
	})

	t.Run("invalid project", func(t *testing.T) {
		_, err := ProjectPrefix("default", "")
		assert.Error(t, err)
	})

	t.Run("empty backupID", func(t *testing.T) {
		_, err := GenerationPrefix("default", "blog", "")
		assert.ErrorContains(t, err, "backupID must not be empty")
	})

	t.Run("invalid volume name", func(t *testing.T) {
		_, err := ArchiveKey("default", "blog", "20260724T140000Z-6e27c7b2", "../../etc/passwd")
		assert.Error(t, err)
	})
}
