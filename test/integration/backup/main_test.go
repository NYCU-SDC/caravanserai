//go:build e2e

// Package backup contains integration tests that exercise the object store
// client and the Managed volume backup flow against a real S3-compatible
// server (MinIO).
//
// These live apart from the API integration tests so a MinIO container is
// only started for the tests that need one.
package backup

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

var verbose = flag.Bool("verbose", false, "enable verbose infrastructure logging")

// shared holds test infrastructure initialised once in TestMain.
var shared struct {
	endpoint string
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	flag.Parse()

	endpoint, cleanup, err := startMinio()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup integration: start minio: %v\n", err)
		return 1
	}
	defer cleanup()

	shared.endpoint = endpoint
	if *verbose {
		fmt.Fprintf(os.Stderr, "backup integration: minio at %s\n", endpoint)
	}

	return m.Run()
}
