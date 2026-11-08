// Package atomicfile replaces a file's contents so that a reader never sees a
// partially written file.
//
// Readers in this codebase watch files from another process: Headscale reloads
// its extra records file, and Traefik reloads route files from a directory.
// WriteFile writes to a temporary file in the target's directory and renames
// it over the target, so a watcher sees either the old or the new contents.
//
// The temporary file name starts with a dot and ends in random digits. It
// never ends in the target's extension, so a watcher that only reads "*.yml"
// or "*.json" files ignores it.
package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile writes data to path with permission perm, replacing any existing
// file atomically. It reports whether it wrote the file.
//
// When path already holds exactly data, WriteFile leaves the file untouched
// and returns false. Watchers then see no change event, and the file keeps
// its modification time.
//
// The temporary file is created in the same directory as path, because a
// rename is only atomic within one file system. On any failure the temporary
// file is removed and path keeps its previous contents.
func WriteFile(path string, data []byte, perm fs.FileMode) (bool, error) {
	same, err := hasContents(path, data)
	if err != nil {
		return false, err
	}
	if same {
		return false, nil
	}

	if err := replace(path, data, perm); err != nil {
		return false, err
	}
	return true, nil
}

// hasContents reports whether path exists and holds exactly data.
func hasContents(path string, data []byte) (bool, error) {
	current, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("atomicfile: read %q: %w", path, err)
	}
	return bytes.Equal(current, data), nil
}

// replace writes data to a temporary file next to path and renames it over
// path.
func replace(path string, data []byte, perm fs.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, tempPattern(base))
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()

	if err := writeAndClose(tmp, data, perm); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: write temp file for %q: %w", path, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: rename temp file to %q: %w", path, err)
	}
	return nil
}

// tempPattern returns the os.CreateTemp pattern for a target named base.
// CreateTemp replaces the trailing "*" with random digits, so the name is
// hidden, ends in digits, and never ends in the target's extension.
func tempPattern(base string) string {
	return "." + base + ".tmp-*"
}

// writeAndClose writes data to f, sets its permission, flushes it to disk and
// closes it. It always closes f, even when an earlier step fails.
func writeAndClose(f *os.File, data []byte, perm fs.FileMode) error {
	_, writeErr := f.Write(data)
	if writeErr == nil {
		// CreateTemp always uses 0600; set the caller's permission explicitly.
		writeErr = f.Chmod(perm)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
