//go:build e2e

package pgbackrest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const minioImage = "cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1"

// tlsMinio starts MinIO with a certificate for the name "minio" on network,
// creates bucket, and returns the CA that signed the certificate.
func tlsMinio(t *testing.T, name, network, bucket string) string {
	t.Helper()
	certs := t.TempDir()
	out, err := dockerRun(t, "run", "--rm", "--user", "0", "--entrypoint", "sh", "-v", certs+":/c", image(), "-c", `
		set -e; cd /c
		openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=test-s3-ca -keyout ca.key -out ca.crt 2>/dev/null
		openssl req -newkey rsa:2048 -nodes -subj /CN=minio -keyout private.key -out s.csr 2>/dev/null
		printf 'subjectAltName=DNS:minio\n' > ext
		openssl x509 -req -in s.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile ext -out public.crt 2>/dev/null
		rm -f ca.key s.csr ext ca.srl; chmod 644 *`)
	require.NoError(t, err, out)

	out, err = dockerRun(t, "run", "-d", "--name", name, "--network", network, "--network-alias", "minio",
		"-e", "MINIO_ROOT_USER=caraadmin", "-e", "MINIO_ROOT_PASSWORD=caraadmin123",
		"-e", "MC_HOST_local=https://caraadmin:caraadmin123@127.0.0.1:9000",
		"-v", certs+":/certs:ro", minioImage, "server", "/data", "--certs-dir", "/certs")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = dockerRun(t, "rm", "-fv", name) })

	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err = dockerRun(t, "exec", name, "mc", "--insecure", "mb", "--ignore-existing", "local/"+bucket)
		if err == nil {
			break
		}
		require.True(t, time.Now().Before(deadline), "MinIO did not come up: %s", out)
		time.Sleep(time.Second)
	}
	return filepath.Join(certs, "ca.crt")
}

// clientInfo runs `pgbackrest info` for stanza from a fresh container on
// network, holding the identity in identityDir, and returns its output.
func clientInfo(t *testing.T, network, identityDir, stanza string) string {
	t.Helper()
	conf := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(conf, "pgbackrest.conf"), []byte(fmt.Sprintf(`[global]
repo1-host=%s
repo1-host-type=tls
repo1-host-ca-file=/etc/cara/pgbackrest/tls/ca.crt
repo1-host-cert-file=/etc/cara/pgbackrest/tls/client.crt
repo1-host-key-file=/etc/cara/pgbackrest/tls/client.key
log-level-console=warn
log-level-file=off
`, pgbackrest.RepoAlias)), 0o644))
	out, _ := dockerRun(t, "run", "--rm", "--network", network, "--user", "999:999",
		"-v", conf+":/etc/pgbackrest:ro", "-v", identityDir+":/etc/cara/pgbackrest/tls:ro",
		image(), "pgbackrest", "--stanza="+stanza, "info")
	return out
}

// The repo host serves each Project only its own stanzas, over that Project's
// network, as an unprivileged user, and takes a new authorization without
// being recreated.
func TestRepoHostServesEachProjectOnlyItsOwnStanzas(t *testing.T) {
	requireImage(t)
	ctx := context.Background()
	runtime := newRuntime(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)

	repoName := "cara-pgbackrest-repo-test-" + suffix
	repoNet := "cara-pgbackrest-test-" + suffix
	netA, netB := "cara-test-a-"+suffix, "cara-test-b-"+suffix
	for _, n := range []string{netA, netB} {
		out, err := dockerRun(t, "network", "create", n)
		require.NoError(t, err, out)
	}
	require.NoError(t, runtime.EnsureNodeNetwork(ctx, repoNet))
	t.Cleanup(func() {
		_ = runtime.RemoveContainer(context.Background(), repoName)
		for _, n := range []string{netA, netB, repoNet} {
			_, _ = dockerRun(t, "network", "rm", n)
		}
	})
	caFile := tlsMinio(t, "cara-test-minio-"+suffix, repoNet, "cara-test")

	dataRoot := t.TempDir()
	t.Cleanup(func() {
		// Files handed to UID 999 cannot be removed by a non-root test.
		_, _ = runtime.RunToCompletion(context.Background(), image(), "0",
			[]string{dataRoot + ":/target"}, []string{"sh", "-c", "rm -rf /target/pgbackrest"})
	})
	host := pgbackrest.NewRepoHost(runtime, pgbackrest.Config{
		DataRoot: dataRoot,
		NodeName: "pve1",
		S3: pgbackrest.S3{
			Endpoint: "https://minio:9000", Bucket: "cara-test",
			AccessKey: "caraadmin", SecretKey: "caraadmin123", CAFile: caFile,
		},
		ContainerName: repoName,
		NetworkName:   repoNet,
	}, zap.NewNop())

	require.NoError(t, host.Authorize(ctx, "uid-a", pgbackrest.Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: netA}, image()))
	firstID, err := dockerRun(t, "inspect", "-f", "{{.Id}}", repoName)
	require.NoError(t, err)
	require.NoError(t, host.Authorize(ctx, "uid-b", pgbackrest.Authorization{Stanzas: []string{"uid-b-pgdata"}, Network: netB}, image()))
	secondID, err := dockerRun(t, "inspect", "-f", "{{.Id}}", repoName)
	require.NoError(t, err)
	assert.Equal(t, firstID, secondID, "a new authorization is a reload, not a new container")

	identityA, err := host.IssueClientIdentity(ctx, "uid-a", image())
	require.NoError(t, err)

	// Its own stanza: reachable through the alias and authorized. The stanza
	// does not exist yet, which is what the server reports.
	own := clientInfo(t, netA, identityA, "uid-a-pgdata")
	assert.NotContains(t, own, "access denied", own)
	assert.Contains(t, own, "missing stanza path", own)

	// Another Project's stanza, with the same certificate: refused.
	other := clientInfo(t, netA, identityA, "uid-b-pgdata")
	assert.Contains(t, other, "access denied", other)

	// Revoked: its own stanza is refused too.
	require.NoError(t, host.Revoke(ctx, "uid-a", netA))
	revoked := clientInfo(t, netB, identityA, "uid-a-pgdata")
	assert.Contains(t, revoked, "access denied", revoked)
	attached, err := runtime.NetworkAttached(ctx, netA, repoName)
	require.NoError(t, err)
	assert.False(t, attached, "the repo host leaves a revoked Project's network")

	// Unprivileged: no process in the repo host runs as root.
	res, err := runtime.Exec(ctx, repoName, "999:999", []string{"sh", "-c",
		`for d in /proc/[0-9]*; do u=$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" $d/status 2>/dev/null); [ -n "$u" ] && echo "$u"; done`})
	require.NoError(t, err)
	uids := strings.Fields(res.Stdout)
	require.NotEmpty(t, uids)
	assert.NotContains(t, uids, "0", "processes: %v", uids)

	version, err := host.Version(ctx)
	require.NoError(t, err)
	assert.Contains(t, version, "pgBackRest 2.59")
}

// A Project's network cannot be removed while the repo host is attached to it;
// revoking the Project first is what lets its cleanup remove the network.
func TestRevokeLetsTheProjectNetworkBeRemoved(t *testing.T) {
	requireImage(t)
	ctx := context.Background()
	runtime := newRuntime(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)

	repoName := "cara-pgbackrest-repo-test-" + suffix
	repoNet := "cara-pgbackrest-test-" + suffix
	netA := "cara-test-a-" + suffix
	out, err := dockerRun(t, "network", "create", netA)
	require.NoError(t, err, out)
	t.Cleanup(func() {
		_ = runtime.RemoveContainer(context.Background(), repoName)
		for _, n := range []string{netA, repoNet} {
			_, _ = dockerRun(t, "network", "rm", n)
		}
	})
	dataRoot := t.TempDir()
	t.Cleanup(func() {
		_, _ = runtime.RunToCompletion(context.Background(), image(), "0",
			[]string{dataRoot + ":/target"}, []string{"sh", "-c", "rm -rf /target/pgbackrest"})
	})
	host := pgbackrest.NewRepoHost(runtime, pgbackrest.Config{
		DataRoot: dataRoot, NodeName: "pve1",
		// Never contacted: removing a network does not need the object store.
		S3:            pgbackrest.S3{Endpoint: "https://minio:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		ContainerName: repoName, NetworkName: repoNet,
	}, zap.NewNop())
	require.NoError(t, host.Authorize(ctx, "uid-a", pgbackrest.Authorization{Stanzas: []string{"uid-a-pgdata"}, Network: netA}, image()))

	out, err = dockerRun(t, "network", "rm", netA)
	require.Error(t, err, "the attached repo host must block the removal: %s", out)

	require.NoError(t, host.Revoke(ctx, "uid-a", netA))
	left, err := host.Remaining(ctx, "uid-a", netA)
	require.NoError(t, err)
	assert.Empty(t, left)
	out, err = dockerRun(t, "network", "rm", netA)
	assert.NoError(t, err, out)
}
