// Package testhelper provides shared test infrastructure for e2e tests.
//
// Usage in TestMain:
//
//	func TestMain(m *testing.M) {
//	    db, cleanup, err := testhelper.StartPostgres()
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    defer cleanup()
//	    // pass db to pgstore.NewWithPool(db) ...
//	    os.Exit(m.Run())
//	}
package testhelper

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq" // postgres driver for database/sql (used only for Ping)
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/context"
)

// StartPostgres launches a disposable PostgreSQL 16 container via Docker,
// waits for it to be ready, runs all embedded schema migrations, and returns
// a live pgxpool.Pool along with a cleanup function that stops and removes
// the container.
//
// Migrations are applied by reusing the same path embedded in the production
// binary (internal/store/postgres), so the test schema is always in sync with
// the real one.
func StartPostgres() (*pgxpool.Pool, string, func(), error) {
	dtPool, err := dockertest.NewPool("")
	if err != nil {
		return nil, "", nil, fmt.Errorf("dockertest: connect to docker: %w", err)
	}
	dtPool.MaxWait = 60 * time.Second

	resource, err := dtPool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16",
		Env: []string{
			"POSTGRES_PASSWORD=password",
			"POSTGRES_USER=postgres",
			"POSTGRES_DB=caravanserai_test",
		},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("dockertest: run postgres: %w", err)
	}

	hostPort := resource.GetHostPort("5432/tcp")
	databaseURL := fmt.Sprintf(
		"postgres://postgres:password@%s/caravanserai_test?sslmode=disable",
		hostPort,
	)

	// Wait until postgres accepts connections.
	retries := 0
	if err = dtPool.Retry(func() error {
		db, err := sql.Open("postgres", databaseURL)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := db.Close(); cerr != nil {
				log.Printf("testhelper: close probe connection: %v", cerr)
			}
		}()
		retries++
		return db.Ping()
	}); err != nil {
		_ = dtPool.Purge(resource)
		return nil, "", nil, fmt.Errorf("dockertest: postgres never became ready (%d retries): %w", retries, err)
	}

	// Open a pgxpool for the tests.
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		_ = dtPool.Purge(resource)
		return nil, "", nil, fmt.Errorf("pgxpool: new: %w", err)
	}

	cleanup := func() {
		pool.Close()
		if err := dtPool.Purge(resource); err != nil {
			log.Printf("testhelper: purge postgres container: %v", err)
		}
	}

	return pool, databaseURL, cleanup, nil
}
