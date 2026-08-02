package restore

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrArchiveTooLarge is returned when an archive expands past the configured
// budget. Compression ratios of 1000:1 are trivial to construct, so a
// modestly-sized upload can otherwise fill the node's disk during extraction.
var ErrArchiveTooLarge = errors.New("restore: archive exceeds maximum extracted size")

// ErrUnsafePath is returned when an archive entry would write outside the
// destination directory.
var ErrUnsafePath = errors.New("restore: archive entry escapes destination")

// Extract unpacks a .tar.gz produced by internal/agent/backup into destDir,
// refusing anything that would write outside it.
//
// The threat model is not a malicious operator — it is a compromised
// container. Containers write freely into their Managed volume, so whatever
// they plant there is faithfully archived by the backup and arrives here.
// Three classes of entry are therefore rejected rather than trusted:
//
//   - paths containing ".." or absolute paths, which would land outside destDir
//   - symlinks and hardlinks pointing outside destDir, which turn a later
//     innocuous-looking entry into a write to an arbitrary host path
//   - archives that expand past maxBytes, which would fill the disk
//
// Extraction additionally re-checks each entry's parent directory after
// resolving symlinks, so an entry cannot be written *through* a symlink
// created earlier in the same archive.
func Extract(archivePath, destDir string, maxBytes int64) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("restore: open archive %q: %w", archivePath, err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("restore: read gzip %q: %w", archivePath, err)
	}
	defer func() { _ = gz.Close() }()

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("restore: create destination %q: %w", destDir, err)
	}
	// Resolve destDir once so containment checks compare like with like —
	// on macOS /var is itself a symlink, and an unresolved prefix check would
	// reject every entry.
	root, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return fmt.Errorf("restore: resolve destination %q: %w", destDir, err)
	}

	tr := tar.NewReader(gz)
	var written int64

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("restore: read archive %q: %w", archivePath, err)
		}

		target, err := safeTargetPath(root, header.Name)
		if err != nil {
			return fmt.Errorf("restore: %q in %q: %w", header.Name, archivePath, err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("restore: create dir %q: %w", target, err)
			}

		case tar.TypeReg:
			n, err := extractFile(tr, target, root, header, maxBytes-written)
			if err != nil {
				return err
			}
			written += n

		case tar.TypeSymlink:
			if err := checkLinkTarget(root, target, header.Linkname); err != nil {
				return fmt.Errorf("restore: symlink %q → %q: %w", header.Name, header.Linkname, err)
			}
			if err := ensureParent(target, root); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("restore: create symlink %q: %w", target, err)
			}

		case tar.TypeLink:
			// A hardlink's target is another entry in this archive, named
			// relative to the archive root.
			linkTarget, err := safeTargetPath(root, header.Linkname)
			if err != nil {
				return fmt.Errorf("restore: hardlink %q → %q: %w", header.Name, header.Linkname, err)
			}
			if err := ensureParent(target, root); err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("restore: create hardlink %q: %w", target, err)
			}

		default:
			// Device nodes, FIFOs and sockets have no legitimate place in a
			// volume archive and are the kind of thing worth refusing loudly.
			return fmt.Errorf("restore: %q in %q: unsupported entry type %q",
				header.Name, archivePath, header.Typeflag)
		}
	}
}

// safeTargetPath joins name onto root and confirms the result stays inside it.
func safeTargetPath(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path", ErrUnsafePath)
	}

	target := filepath.Join(root, name)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: resolves to %q", ErrUnsafePath, target)
	}
	return target, nil
}

// checkLinkTarget rejects a symlink whose target lands outside root.
//
// An absolute target is refused outright: inside a container it would have
// referred to that container's filesystem, and recreating it on the host
// points somewhere entirely different.
func checkLinkTarget(root, linkPath, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("%w: absolute symlink target", ErrUnsafePath)
	}

	resolved := filepath.Join(filepath.Dir(linkPath), linkname)
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return fmt.Errorf("%w: target resolves to %q", ErrUnsafePath, resolved)
	}
	return nil
}

// ensureParent creates an entry's parent directory and confirms that, after
// resolving any symlinks, it is still inside root.
//
// This is what stops the two-step attack: an archive plants a symlink
// pointing outside, then a later entry writes to a path that traverses it.
// The first entry is caught by checkLinkTarget; this catches the second even
// if a symlink somehow got created another way.
func ensureParent(target, root string) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("restore: create parent %q: %w", parent, err)
	}

	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("restore: resolve parent %q: %w", parent, err)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return fmt.Errorf("%w: parent %q resolves outside destination", ErrUnsafePath, parent)
	}
	return nil
}

// extractFile writes one regular file, refusing to exceed the remaining size
// budget. It returns the number of bytes written.
func extractFile(tr io.Reader, target, root string, header *tar.Header, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, ErrArchiveTooLarge
	}
	if err := ensureParent(target, root); err != nil {
		return 0, err
	}

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, header.FileInfo().Mode().Perm())
	if err != nil {
		return 0, fmt.Errorf("restore: create %q: %w", target, err)
	}
	defer func() { _ = out.Close() }()

	// Copy one byte past the budget so an over-large file is detected rather
	// than silently truncated to the limit.
	n, err := io.Copy(out, io.LimitReader(tr, budget+1))
	if err != nil {
		return n, fmt.Errorf("restore: write %q: %w", target, err)
	}
	if n > budget {
		return n, ErrArchiveTooLarge
	}
	return n, nil
}
