package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFile(t *testing.T) {
	tests := []struct {
		name        string
		existing    []byte // nil means the target does not exist
		data        []byte
		wantWritten bool
	}{
		{name: "creates a missing file", existing: nil, data: []byte("new\n"), wantWritten: true},
		{name: "replaces different contents", existing: []byte("old\n"), data: []byte("new\n"), wantWritten: true},
		{name: "skips identical contents", existing: []byte("same\n"), data: []byte("same\n"), wantWritten: false},
		{name: "writes empty data over a non-empty file", existing: []byte("old\n"), data: []byte{}, wantWritten: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "records.json")
			if tt.existing != nil {
				require.NoError(t, os.WriteFile(path, tt.existing, 0o644))
			}

			written, err := WriteFile(path, tt.data, 0o644)

			require.NoError(t, err)
			assert.Equal(t, tt.wantWritten, written)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.data, got)
			assertOnlyEntries(t, dir, "records.json")
		})
	}
}

func TestWriteFile_IdenticalContentsKeepModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	require.NoError(t, os.WriteFile(path, []byte("same\n"), 0o644))
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, past, past))

	written, err := WriteFile(path, []byte("same\n"), 0o644)

	require.NoError(t, err)
	assert.False(t, written)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past), "mod time changed to %v", info.ModTime())
}

func TestWriteFile_SetsPermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	path := filepath.Join(t.TempDir(), "records.json")

	_, err := WriteFile(path, []byte("data\n"), 0o640)

	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestWriteFile_Errors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the target path
	}{
		{
			name: "target directory does not exist",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "missing", "records.json")
			},
		},
		{
			name: "target is a non-empty directory so rename fails",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "records.json")
				require.NoError(t, os.MkdirAll(filepath.Join(path, "child"), 0o755))
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.setup(t, dir)

			written, err := WriteFile(path, []byte("data\n"), 0o644)

			require.Error(t, err)
			assert.False(t, written)
			assertNoTempFiles(t, dir)
		})
	}
}

func TestTempPattern_AvoidsTargetExtension(t *testing.T) {
	// A directory watcher that reads "*.yml" or "*.json" must never pick up
	// the temporary file while it is still being written.
	tests := []struct {
		base string
		ext  string
	}{
		{base: "blog.yml", ext: ".yml"},
		{base: "records.json", ext: ".json"},
	}

	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			tmp, err := os.CreateTemp(t.TempDir(), tempPattern(tt.base))
			require.NoError(t, err)
			require.NoError(t, tmp.Close())

			name := filepath.Base(tmp.Name())
			assert.True(t, strings.HasPrefix(name, "."), "temp name %q should be hidden", name)
			assert.False(t, strings.HasSuffix(name, tt.ext), "temp name %q must not end in %s", name, tt.ext)
		})
	}
}

func TestWriteFile_RelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	written, err := WriteFile("records.json", []byte("data\n"), 0o644)

	require.NoError(t, err)
	assert.True(t, written)
	assertOnlyEntries(t, dir, "records.json")
}

// assertOnlyEntries fails when dir holds anything other than names.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	assert.ElementsMatch(t, names, got)
}

// assertNoTempFiles fails when dir still holds a temporary file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "temporary file left behind")
	}
}
