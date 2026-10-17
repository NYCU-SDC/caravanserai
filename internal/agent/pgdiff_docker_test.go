//go:build e2e

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"
	"NYCU-SDC/caravanserai/internal/agent/pgbackup"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const pgdiffMinioImage = "cgr.dev/chainguard/minio:latest@sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1"

func pgdiffImage() string {
	if ref := os.Getenv("CARA_POSTGRES_IMAGE"); ref != "" {
		return ref
	}
	return v1.PostgresDifferentialImage
}

func dockerCLI(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// pgdiffMinio starts MinIO over TLS as "minio" on network and returns the CA
// that signed its certificate.
func pgdiffMinio(t *testing.T, name, network, bucket string) string {
	t.Helper()
	certs := t.TempDir()
	out, err := dockerCLI("run", "--rm", "--user", "0", "--entrypoint", "sh", "-v", certs+":/c", pgdiffImage(), "-c", `
		set -e; cd /c
		openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=test-s3-ca -keyout ca.key -out ca.crt 2>/dev/null
		openssl req -newkey rsa:2048 -nodes -subj /CN=minio -keyout private.key -out s.csr 2>/dev/null
		printf 'subjectAltName=DNS:minio\n' > ext
		openssl x509 -req -in s.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile ext -out public.crt 2>/dev/null
		rm -f ca.key s.csr ext ca.srl; chmod 644 *`)
	require.NoError(t, err, out)
	out, err = dockerCLI("run", "-d", "--name", name, "--network", network, "--network-alias", "minio",
		"-e", "MINIO_ROOT_USER=caraadmin", "-e", "MINIO_ROOT_PASSWORD=caraadmin123",
		"-e", "MC_HOST_local=https://caraadmin:caraadmin123@127.0.0.1:9000",
		"-v", certs+":/certs:ro", pgdiffMinioImage, "server", "/data", "--certs-dir", "/certs")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = dockerCLI("rm", "-fv", name) })
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err = dockerCLI("exec", name, "mc", "--insecure", "mb", "--ignore-existing", "local/"+bucket)
		if err == nil {
			return filepath.Join(certs, "ca.crt")
		}
		require.True(t, time.Now().Before(deadline), "MinIO did not come up: %s", out)
		time.Sleep(time.Second)
	}
}

// A PostgresDifferential Project placed through the poll loop's reconcile
// reaches Running with its database archiving WAL through the Node's repo
// host into the object store.
func TestPostgresPlacementArchivesThroughTheRepoHost(t *testing.T) {
	if _, err := dockerCLI("image", "inspect", pgdiffImage()); err != nil {
		t.Skipf("%s is not present locally; skipping real-Docker test", pgdiffImage())
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	dataRoot := t.TempDir()

	runtime, err := docker.NewDockerRuntime("unix:///var/run/docker.sock", dataRoot, true, zap.NewNop())
	require.NoError(t, err)

	project := func() *v1.Project {
		p := pgProject()
		p.Name = "pgdiff-it-" + suffix
		p.UID = "uid-it-" + suffix
		p.Spec.Services = p.Spec.Services[:1]
		p.Spec.Services[0].Image = pgdiffImage()
		p.Spec.Services[0].Env = []v1.EnvVar{{Name: "POSTGRES_PASSWORD", Value: "cara"}}
		return p
	}
	p := project()

	repoName, repoNet := "cara-pgbackrest-repo-it-"+suffix, "cara-pgbackrest-it-"+suffix
	require.NoError(t, runtime.EnsureNodeNetwork(ctx, repoNet))
	t.Cleanup(func() {
		_, _ = dockerCLI("rm", "-fv", docker.ContainerName(p.Name, "db"))
		_, _ = dockerCLI("rm", "-fv", repoName)
		_, _ = dockerCLI("network", "rm", docker.NetworkName(p.Name))
		_, _ = dockerCLI("network", "rm", repoNet)
		// Files handed to UID 999 cannot be removed by a non-root test.
		_, _ = runtime.RunToCompletion(context.Background(), pgdiffImage(), "0",
			[]string{dataRoot + ":/target"}, []string{"sh", "-c", "rm -rf /target/pgbackrest /target/volumes"})
	})
	caFile := pgdiffMinio(t, "cara-minio-it-"+suffix, repoNet, "cara-it")

	repo := pgbackrest.NewRepoHost(runtime, pgbackrest.Config{
		DataRoot: dataRoot,
		NodeName: gateNode,
		S3: pgbackrest.S3{
			Endpoint: "https://minio:9000", Bucket: "cara-it",
			AccessKey: "caraadmin", SecretKey: "caraadmin123", CAFile: caFile,
		},
		ContainerName: repoName,
		NetworkName:   repoNet,
	}, zap.NewNop())

	gs, client := newGateServer(t, project())
	backups := &BackupSupport{
		Coordinator: backup.NewCoordinator(),
		Restorer:    restore.NewRestorer(nil, restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop()),
		DataRoot:    dataRoot,
		NodeName:    gateNode,
		RepoHost:    repo,
	}

	reconcileOne(ctx, client, runtime, &recordingRoutes{}, backups, p, zap.NewNop())

	if blocks := gs.writes("patch", v1.ConditionTypeRecoveryBlocked); len(blocks) > 0 {
		t.Fatalf("placement blocked: %s: %s", blocks[0].Reason, blocks[0].Message)
	}
	require.Len(t, gs.updates, 1)
	require.Equal(t, v1.ProjectPhaseRunning, gs.updates[0].Phase, gs.updates[0].Message)
	assert.Equal(t, []string{"pgdata"}, gs.stanzas)

	db := docker.ContainerName(p.Name, "db")
	stanza := v1.StanzaName(p.UID, "pgdata")
	info, err := runtime.Exec(ctx, db, "postgres", []string{"pgbackrest", "--stanza=" + stanza, "--output=json", "info"})
	require.NoError(t, err)
	require.Zero(t, info.ExitCode, info.Stderr)
	assert.Contains(t, info.Stdout, `"status":{"code":2`, "a stanza with no backup yet: %s", info.Stdout)
	assert.Contains(t, info.Stdout, `"archive":[{`, "WAL has reached the repository: %s", info.Stdout)

	// A second tick finds the database archiving and leaves it alone.
	running, err := client.GetProject(ctx, p.Name)
	require.NoError(t, err)
	before, err := dockerCLI("inspect", "-f", "{{.State.StartedAt}}", db)
	require.NoError(t, err)
	require.NoError(t, finishPostgresDifferential(ctx, client, repo, runtime, running))
	after, err := dockerCLI("inspect", "-f", "{{.State.StartedAt}}", db)
	require.NoError(t, err)
	assert.Equal(t, before, after, "finishing again does not restart a database that already archives")

	// Backups while PostgreSQL runs: full first, then differential.
	rec := &itemRecorder{}
	runner := pgbackup.NewRunner(backups.Coordinator, runtime, pgbackup.NewFileContinuity(dataRoot, runtime), rec, zap.NewNop())
	running.Status.Phase = v1.ProjectPhaseRunning
	sql := func(q string) string {
		out, err := psql(ctx, runtime, db, q)
		require.NoError(t, err, out)
		return out
	}
	sql("CREATE TABLE notes (id int)")
	sql("INSERT INTO notes SELECT generate_series(1, 1000)")
	require.NoError(t, runner.Run(ctx, running))
	sql("INSERT INTO notes SELECT generate_series(1001, 2000)")
	require.NoError(t, runner.Run(ctx, running))
	require.Len(t, rec.items, 2)
	assert.Equal(t, pgbackup.TypeFull, rec.items[0][0].Type)
	assert.Equal(t, pgbackup.TypeDiff, rec.items[1][0].Type,
		"the log continued from the first backup to the second, so continuity is known")
	logs, err := runtime.LogLines(ctx, db)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(logs, "\n"), "archive-push",
		"archive-push reports through PostgreSQL's log, where a dropped-WAL warning would appear")
	assert.True(t, strings.HasPrefix(rec.items[1][0].Label, rec.items[0][0].Label+"_"),
		"the differential depends on the full: %s, %s", rec.items[0][0].Label, rec.items[1][0].Label)
	assert.Equal(t, 16, rec.items[0][0].PGMajor)
	state, err := dockerCLI("inspect", "-f", "{{.State.Running}}", db)
	require.NoError(t, err)
	assert.Equal(t, "true", state, "a backup does not stop the database")

	// A cancelled run leaves no backup process behind.
	cancelCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	require.ErrorIs(t, runner.Run(cancelCtx, running), context.Canceled, "the run was cancelled mid-backup")
	require.Eventually(t, func() bool {
		res, err := runtime.Exec(ctx, db, "postgres", []string{"sh", "-c",
			`for d in /proc/[0-9]*; do tr '\0' ' ' < $d/cmdline 2>/dev/null; echo; done | grep -c '^pgbackrest.* backup' || true`})
		return err == nil && strings.TrimSpace(res.Stdout) == "0"
	}, 30*time.Second, 500*time.Millisecond, "no pgbackrest backup process is left in the container")
	// And the next run is not blocked by it.
	require.NoError(t, runner.Run(ctx, running))
}

type itemRecorder struct{ items [][]pgbackup.Item }

func (r *itemRecorder) Record(_ context.Context, _ *v1.Project, _ string, items []pgbackup.Item) error {
	r.items = append(r.items, items)
	return nil
}
