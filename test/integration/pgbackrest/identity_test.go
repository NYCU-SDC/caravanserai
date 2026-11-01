//go:build e2e

package pgbackrest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Project identity issued by the Agent, even one not running as root, ends
// up readable by pgBackRest in the image and by nobody else in it.
func TestIdentityIsReadableOnlyByTheImageUser(t *testing.T) {
	requireImage(t)
	ctx := context.Background()
	runtime := newRuntime(t)

	root := t.TempDir()
	ca, err := pgbackrest.LoadOrCreateCA(filepath.Join(root, "ca"), "pve1", time.Now())
	require.NoError(t, err)
	dir := filepath.Join(root, "projects", "uid-1")
	files, err := ca.IssueClient(dir, "uid-1", time.Now())
	require.NoError(t, err)
	require.NoError(t, pgbackrest.NewChowner(runtime, image()).Give(ctx, pgbackrest.ImageOwner, files...))
	t.Cleanup(func() {
		// The files now belong to 999; let a helper remove them so the
		// temporary directory can be cleaned up by a non-root test.
		_, _ = runtime.RunToCompletion(context.Background(), image(), "0",
			[]string{dir + ":/target"}, []string{"sh", "-c", "rm -f /target/*"})
	})

	read := func(user string) string {
		out, _ := dockerRun(t, "run", "--rm", "--user", user, "--entrypoint", "sh",
			"-v", dir+":/etc/cara/pgbackrest/tls:ro", image(),
			"-c", "if cat /etc/cara/pgbackrest/tls/client.key >/dev/null 2>&1; then echo readable; else echo denied; fi")
		return out
	}
	assert.Equal(t, "readable", read("999:999"))
	assert.Equal(t, "denied", read("1000:1000"))

	// A reissue by the same, possibly non-root, Agent still works: the
	// directory stays the Agent's even though the files are not.
	_, err = ca.IssueClient(dir, "uid-1", time.Now().Add(400*24*time.Hour))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "client.crt"))
	require.NoError(t, err)
}
