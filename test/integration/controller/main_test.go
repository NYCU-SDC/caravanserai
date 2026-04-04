//go:build e2e

// Package controller contains controller integration tests that validate
// multiple controllers working together through a real PostgreSQL store,
// real event bus, and real controller manager.
package controller

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

var verbose = flag.Bool("verbose", false, "enable verbose infrastructure logging")

// shared holds test infrastructure initialised once in TestMain.
var shared struct {
	pool        *pgxpool.Pool
	databaseURL string
	logger      *zap.Logger
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	flag.Parse()

	pool, databaseURL, cleanup, err := testhelper.StartPostgres()
	if err != nil {
		fmt.Fprintf(os.Stderr, "controller integration: start postgres: %v\n", err)
		return 1
	}
	defer cleanup()

	var logger *zap.Logger
	if *verbose {
		cfg := zap.NewDevelopmentConfig()
		cfg.OutputPaths = []string{"stderr"}
		cfg.ErrorOutputPaths = []string{"stderr"}
		logger, err = cfg.Build()
		if err != nil {
			fmt.Fprintf(os.Stderr, "controller integration: build logger: %v\n", err)
			return 1
		}
		defer func() { _ = logger.Sync() }()
	} else {
		logger = zap.NewNop()
	}

	shared.pool = pool
	shared.databaseURL = databaseURL
	shared.logger = logger

	return m.Run()
}
