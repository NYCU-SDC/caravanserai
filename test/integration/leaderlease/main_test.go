//go:build e2e

// Package leaderlease checks the control-plane leader lease's conditional
// writes against a real S3-compatible server (MinIO). The unit tests in
// internal/server/leaderlease use an in-memory store that imitates S3; these
// confirm the imitation matches what a server actually does.
package leaderlease

import (
	"fmt"
	"os"
	"testing"

	"NYCU-SDC/caravanserai/test/integration/testhelper"
)

var shared struct {
	endpoint string
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	endpoint, cleanup, err := testhelper.StartMinio()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leaderlease integration: start minio: %v\n", err)
		return 1
	}
	defer cleanup()
	shared.endpoint = endpoint
	return m.Run()
}
