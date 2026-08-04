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

// Extract unpacks a .tar.gz produced by internal/agent/backup into destDir.
//
// The threat model is not a malicious operator — it is a compromised
// container. Containers write freely into their Managed volume, so whatever
// they plant there is faithfully archived by the backup (symlinks included,
// see backup.archiveVolume) and arrives here.
//
// Containment is delegated to os.Root rather than hand-rolled path checks.
// Every write goes through a handle to destDir whose methods refuse any name
// resolving outside it, symlinks included — on Linux that is the kernel doing
// the resolution. This matters because the previous approach validated paths
// per entry type, and safety then depended on remembering to call the check in
// every branch of the switch below. Two branches did not, and an archive that
// planted `a -> "."` followed by `a/b -> ".."` could reach one directory above
// destDir: each check passed lexically because `a` looked like a subdirectory
// while actually being the root itself. With os.Root the escape is not blocked
// by a check, it is unrepresentable.
//
// Two things os.Root deliberately does not do, which are still handled here:
//
//   - It does not validate a symlink's target at creation time, only traversal
//     through it later. A stored link that points outside the volume is refused
//     outright: an absolute target meant something inside the container's
//     filesystem and means something else entirely on the host.
//   - It has no notion of an extraction budget, so archive bombs are capped
//     separately.
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
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("restore: open destination %q: %w", destDir, err)
	}
	defer func() { _ = root.Close() }()

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

		// A lexical pre-check, so the obvious cases fail with a clear error
		// naming the offending entry. os.Root is what actually enforces
		// containment for everything that gets past this.
		name, err := checkEntryName(header.Name)
		if err != nil {
			return fmt.Errorf("restore: %q in %q: %w", header.Name, archivePath, err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, header.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("restore: create dir %q: %w", header.Name, err)
			}

		case tar.TypeReg:
			n, err := extractFile(tr, root, name, header, maxBytes-written)
			if err != nil {
				return err
			}
			written += n

		case tar.TypeSymlink:
			if err := checkSymlinkTarget(name, header.Linkname); err != nil {
				return fmt.Errorf("restore: symlink %q → %q: %w", header.Name, header.Linkname, err)
			}
			if err := ensureParent(root, name); err != nil {
				return err
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return fmt.Errorf("restore: create symlink %q: %w", header.Name, err)
			}

		case tar.TypeLink:
			// A hardlink's target is another entry in this archive, named
			// relative to the archive root.
			linkTarget, err := checkEntryName(header.Linkname)
			if err != nil {
				return fmt.Errorf("restore: hardlink %q → %q: %w", header.Name, header.Linkname, err)
			}
			if err := ensureParent(root, name); err != nil {
				return err
			}
			if err := root.Link(linkTarget, name); err != nil {
				return fmt.Errorf("restore: create hardlink %q: %w", header.Name, err)
			}

		default:
			// Device nodes, FIFOs and sockets have no legitimate place in a
			// volume archive and are the kind of thing worth refusing loudly.
			return fmt.Errorf("restore: %q in %q: unsupported entry type %q",
				header.Name, archivePath, header.Typeflag)
		}
	}
}

// checkEntryName rejects entry names that are obviously outside the
// destination and returns the cleaned, root-relative form.
//
// os.Root would refuse these too, but with an error naming the syscall rather
// than the archive entry. Catching them here keeps the failure legible when an
// operator has to work out which archive is bad.
func checkEntryName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty entry name", ErrUnsafePath)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path", ErrUnsafePath)
	}

	cleaned := filepath.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: resolves to %q", ErrUnsafePath, cleaned)
	}
	return cleaned, nil
}

// checkSymlinkTarget rejects a symlink whose target lands outside the volume.
//
// os.Root allows creating such a link — it only refuses to traverse one — but
// storing it is still wrong. An absolute target referred to the container's
// filesystem when it was created and points somewhere unrelated on the host,
// and a relative one that climbs out of the volume cannot survive the volume
// being restored onto a different node.
func checkSymlinkTarget(entryName, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("%w: empty symlink target", ErrUnsafePath)
	}
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("%w: absolute symlink target", ErrUnsafePath)
	}

	resolved := filepath.Join(filepath.Dir(entryName), linkname)
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%w: target resolves to %q", ErrUnsafePath, resolved)
	}
	return nil
}

// ensureParent creates an entry's parent directory. Archives do not reliably
// carry an entry for every directory they reference.
func ensureParent(root *os.Root, name string) error {
	parent := filepath.Dir(name)
	if parent == "." || parent == string(os.PathSeparator) {
		return nil
	}
	if err := root.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("restore: create parent %q: %w", parent, err)
	}
	return nil
}

// extractFile writes one regular file, refusing to exceed the remaining size
// budget. It returns the number of bytes written.
func extractFile(tr io.Reader, root *os.Root, name string, header *tar.Header, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, ErrArchiveTooLarge
	}
	if err := ensureParent(root, name); err != nil {
		return 0, err
	}

	out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, header.FileInfo().Mode().Perm())
	if err != nil {
		return 0, fmt.Errorf("restore: create %q: %w", name, err)
	}
	defer func() { _ = out.Close() }()

	// Copy one byte past the budget so an over-large file is detected rather
	// than silently truncated to the limit.
	n, err := io.Copy(out, io.LimitReader(tr, budget+1))
	if err != nil {
		return n, fmt.Errorf("restore: write %q: %w", name, err)
	}
	if n > budget {
		return n, ErrArchiveTooLarge
	}
	return n, nil
}
