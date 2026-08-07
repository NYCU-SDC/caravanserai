package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ArchiveResult describes a completed archive: what BuildManifest and the
// upload step need to know about it.
type ArchiveResult struct {
	SizeBytes int64
	SHA256    string
}

// Archive tars and gzips every file under sourceDir into destFile, computing
// the archive's sha256 as it writes. destFile's directory must already
// exist; Archive does not create it.
//
// Symlinks are archived as symlinks — their target path is stored, but the
// target is never read or followed. sourceDir is agent-owned, but the
// containers that write into it are not: a compromised container could plant
// a symlink pointing outside the volume directory, and following it would
// let an archive silently include arbitrary host files.
func Archive(sourceDir, destFile string) (ArchiveResult, error) {
	out, err := os.Create(destFile)
	if err != nil {
		return ArchiveResult{}, fmt.Errorf("backup: create archive %q: %w", destFile, err)
	}
	defer func() { _ = out.Close() }()

	hasher := sha256.New()
	counter := &countingWriter{}
	gz := gzip.NewWriter(io.MultiWriter(out, hasher, counter))
	tw := tar.NewWriter(gz)

	if err := archiveDir(tw, sourceDir); err != nil {
		return ArchiveResult{}, err
	}
	if err := tw.Close(); err != nil {
		return ArchiveResult{}, fmt.Errorf("backup: close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return ArchiveResult{}, fmt.Errorf("backup: close gzip writer: %w", err)
	}
	if err := out.Sync(); err != nil {
		return ArchiveResult{}, fmt.Errorf("backup: sync archive %q: %w", destFile, err)
	}

	return ArchiveResult{
		SizeBytes: counter.n,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// archiveDir walks sourceDir and writes each entry to tw with paths relative
// to sourceDir, so the archive's root corresponds to the volume directory's
// contents rather than its host-specific absolute path.
func archiveDir(tw *tar.Writer, sourceDir string) error {
	return filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("backup: walk %q: %w", path, err)
		}

		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("backup: relativize %q: %w", path, err)
		}
		if rel == "." {
			// The root directory itself is not represented as a tar entry.
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("backup: stat %q: %w", path, err)
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("backup: read symlink %q: %w", path, err)
			}
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("backup: build tar header for %q: %w", path, err)
		}
		header.Name = filepath.ToSlash(rel)

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("backup: write tar header for %q: %w", path, err)
		}

		// Regular files are the only entries with a body to copy. Symlinks
		// carry their target in the header only; directories have no body.
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("backup: open %q: %w", path, err)
			}
			_, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("backup: archive %q: %w", path, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("backup: close %q: %w", path, closeErr)
			}
		}
		return nil
	})
}

// countingWriter counts bytes written, so Archive can report the compressed
// archive's exact size without a second pass over the output file.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
