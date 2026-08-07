package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readTarEntries extracts every entry's name and content from a .tar.gz file,
// for asserting on archive contents without depending on Archive's own logic.
func readTarEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gz.Close()

	tr := tar.NewReader(gz)
	entries := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			entries[hdr.Name] = string(body)
		} else {
			entries[hdr.Name] = "<" + string(hdr.Typeflag) + ">"
		}
	}
	return entries
}

func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "PG_VERSION"), []byte("17"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "base", "1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "base", "1", "data"), []byte("row bytes"), 0o600))

	dest := filepath.Join(t.TempDir(), "db-data.tar.gz")
	result, err := Archive(src, dest)
	require.NoError(t, err)

	assert.Positive(t, result.SizeBytes)
	assert.Len(t, result.SHA256, 64, "sha256 hex digest is 64 chars")

	entries := readTarEntries(t, dest)
	assert.Equal(t, "17", entries["PG_VERSION"])
	assert.Equal(t, "row bytes", entries["base/1/data"])
	// The source directory's own absolute path must not leak into the archive.
	for name := range entries {
		assert.NotContains(t, name, src)
	}
}

func TestArchiveReportedSizeMatchesFile(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello world"), 0o600))

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	result, err := Archive(src, dest)
	require.NoError(t, err)

	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), result.SizeBytes)
}

func TestArchiveChecksumMatchesFileContent(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello world"), 0o600))

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	result, err := Archive(src, dest)
	require.NoError(t, err)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), result.SHA256)
}

func TestArchiveEmptyDirectory(t *testing.T) {
	src := t.TempDir()

	dest := filepath.Join(t.TempDir(), "empty.tar.gz")
	result, err := Archive(src, dest)
	require.NoError(t, err)
	assert.Positive(t, result.SizeBytes, "gzip framing means even an empty archive is non-zero bytes")

	entries := readTarEntries(t, dest)
	assert.Empty(t, entries)
}

func TestArchiveDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	// Simulate a compromised container planting a symlink that points
	// outside the volume directory, at a file the archive must never read.
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "host-secret.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("should never appear in the archive"), 0o600))

	src := t.TempDir()
	linkPath := filepath.Join(src, "escape")
	require.NoError(t, os.Symlink(secretPath, linkPath))

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	_, err := Archive(src, dest)
	require.NoError(t, err)

	entries := readTarEntries(t, dest)
	// The symlink itself is archived as a symlink entry (marked "<2>" for
	// tar.TypeSymlink), never as a regular file containing the target's data.
	assert.Equal(t, "<2>", entries["escape"])
	for _, content := range entries {
		assert.NotContains(t, content, "should never appear in the archive")
	}
}

func TestArchiveFailsWhenSourceDoesNotExist(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	_, err := Archive(filepath.Join(t.TempDir(), "does-not-exist"), dest)
	assert.Error(t, err)
}
