package restore

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry describes one entry to write into a test archive.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

// buildArchive writes a .tar.gz containing exactly the given entries,
// including ones a well-behaved archiver would never produce — the point is to
// exercise what happens when a hostile archive arrives.
func buildArchive(t *testing.T, path string, entries []tarEntry) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o600
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if e.typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
}

func TestExtractRoundTrip(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "vol.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "PG_VERSION", typeflag: tar.TypeReg, body: "17"},
		{name: "base", typeflag: tar.TypeDir, mode: 0o700},
		{name: "base/1", typeflag: tar.TypeReg, body: "row data"},
	})

	dest := filepath.Join(t.TempDir(), "out")
	require.NoError(t, Extract(archive, dest, 1<<20))

	got, err := os.ReadFile(filepath.Join(dest, "PG_VERSION"))
	require.NoError(t, err)
	assert.Equal(t, "17", string(got))

	got, err = os.ReadFile(filepath.Join(dest, "base", "1"))
	require.NoError(t, err)
	assert.Equal(t, "row data", string(got))
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "../escaped.txt", typeflag: tar.TypeReg, body: "pwned"},
	})

	dest := filepath.Join(t.TempDir(), "out")
	err := Extract(archive, dest, 1<<20)

	require.ErrorIs(t, err, ErrUnsafePath)
	_, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt"))
	assert.True(t, os.IsNotExist(statErr), "the traversing entry must never be written")
}

func TestExtractRejectsAbsolutePath(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "/etc/cara-pwned", typeflag: tar.TypeReg, body: "pwned"},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1<<20)
	assert.ErrorIs(t, err, ErrUnsafePath)
}

func TestExtractRejectsEscapingSymlink(t *testing.T) {
	// A container can plant any symlink it likes inside its own volume, and
	// the backup faithfully archives it. One pointing outside the volume must
	// not be recreated on the host.
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "escape", typeflag: tar.TypeSymlink, linkname: "../../../../etc"},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1<<20)
	assert.ErrorIs(t, err, ErrUnsafePath)
}

func TestExtractRejectsAbsoluteSymlinkTarget(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "escape", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1<<20)
	assert.ErrorIs(t, err, ErrUnsafePath)
}

func TestExtractRejectsWriteThroughSymlink(t *testing.T) {
	// The two-step attack: entry 1 creates a symlink to somewhere outside,
	// entry 2 writes to a path that traverses it. Even if the symlink itself
	// were allowed, the write must not land outside the destination.
	outside := t.TempDir()
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "link", typeflag: tar.TypeSymlink, linkname: outside},
		{name: "link/pwned.txt", typeflag: tar.TypeReg, body: "pwned"},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1<<20)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(outside, "pwned.txt"))
	assert.True(t, os.IsNotExist(statErr), "nothing may be written outside the destination")
}

func TestExtractAllowsInternalSymlink(t *testing.T) {
	// A symlink pointing within the volume is legitimate and must survive a
	// backup/restore round trip.
	archive := filepath.Join(t.TempDir(), "vol.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "releases", typeflag: tar.TypeDir, mode: 0o700},
		{name: "releases/v1", typeflag: tar.TypeReg, body: "v1"},
		{name: "current", typeflag: tar.TypeSymlink, linkname: "releases/v1"},
	})

	dest := filepath.Join(t.TempDir(), "out")
	require.NoError(t, Extract(archive, dest, 1<<20))

	target, err := os.Readlink(filepath.Join(dest, "current"))
	require.NoError(t, err)
	assert.Equal(t, "releases/v1", target)
}

func TestExtractRejectsArchiveBomb(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bomb.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "big", typeflag: tar.TypeReg, body: strings.Repeat("A", 4096)},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1024)
	assert.ErrorIs(t, err, ErrArchiveTooLarge)
}

func TestExtractBudgetIsCumulative(t *testing.T) {
	// Several individually-small files must not add up past the cap.
	archive := filepath.Join(t.TempDir(), "bomb.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "a", typeflag: tar.TypeReg, body: strings.Repeat("A", 600)},
		{name: "b", typeflag: tar.TypeReg, body: strings.Repeat("B", 600)},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1000)
	assert.ErrorIs(t, err, ErrArchiveTooLarge)
}

func TestExtractRejectsUnsupportedEntryType(t *testing.T) {
	// Device nodes and FIFOs have no place in a volume archive.
	archive := filepath.Join(t.TempDir(), "odd.tar.gz")
	buildArchive(t, archive, []tarEntry{
		{name: "fifo", typeflag: tar.TypeFifo},
	})

	err := Extract(archive, filepath.Join(t.TempDir(), "out"), 1<<20)
	assert.ErrorContains(t, err, "unsupported entry type")
}
