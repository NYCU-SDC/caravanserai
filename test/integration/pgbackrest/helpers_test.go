//go:build e2e

// Package pgbackrest exercises the Node's pgBackRest repo host and identities
// against the host's Docker and the cara-postgres image. Every test skips,
// saying why, when that image is not present locally.
package pgbackrest

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// image is the cara-postgres reference the tests run; CARA_POSTGRES_IMAGE
// overrides it, as for the image's own smoke test.
func image() string {
	if ref := os.Getenv("CARA_POSTGRES_IMAGE"); ref != "" {
		return ref
	}
	return "ghcr.io/nycu-sdc/cara-postgres:16"
}

// requireImage skips the test when the image is not on the host.
func requireImage(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", image()).Run(); err != nil {
		t.Skipf("%s is not present locally (make build-cara-postgres-image); skipping real-Docker test", image())
	}
}

func newRuntime(t *testing.T) *docker.DockerRuntime {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	r, err := docker.NewDockerRuntime(host, t.TempDir(), zap.NewNop())
	require.NoError(t, err)
	return r
}

// dockerRun runs docker with args and returns trimmed combined output.
func dockerRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
