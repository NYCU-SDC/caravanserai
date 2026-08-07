//go:build e2e

package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// readTarGz extracts entry names and regular-file contents from a gzipped tar
// stream, so tests can assert on what an archive actually holds.
func readTarGz(t *testing.T, r io.Reader) (names []string, contents []string) {
	t.Helper()

	gz, err := gzip.NewReader(r)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			body, readErr := io.ReadAll(tr)
			require.NoError(t, readErr)
			contents = append(contents, string(body))
		}
	}
	return names, contents
}
