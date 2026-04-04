// Package controllerhelper provides shared test infrastructure for controller
// integration tests.  It wires up a real PostgreSQL store, event bus, store
// adapters, and controller manager so that multiple controllers can be
// validated working together end-to-end (minus a real Agent and Docker).
package controllerhelper

import (
	"context"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	// Fast timings for integration tests.
	testSeedInterval = 1 * time.Second
	testRequeueAfter = 1 * time.Second
	testErrorBackoff = 1 * time.Second
)

// Suite holds all the wired components needed for controller integration
// tests.  Call NewSuite to create one, then Start to launch the controller
// manager in the background.
type Suite struct {
	Store   *pgstore.Store
	Manager *controller.Manager
	Bus     *event.Bus
	Pool    *pgxpool.Pool
	Logger  *zap.Logger
}

// NewSuite wires up a real pgstore, event bus, adapters, and controller
// manager with all 4 controllers registered.  The controller manager is NOT
// started yet — call Start(ctx) to launch it.
//
// ctrlOpts are forwarded to every controller constructor, allowing callers to
// inject a fake clock or custom seed interval.
func NewSuite(t *testing.T, pool *pgxpool.Pool, databaseURL string, logger *zap.Logger, ctrlOpts ...controller.Option) *Suite {
	t.Helper()

	ctx := context.Background()

	bus := event.New(logger, 256)

	pgStore, err := pgstore.New(ctx, databaseURL, logger, bus)
	require.NoError(t, err, "pgstore.New")

	// Merge caller-provided options with the fast seed interval default.
	// If the caller already provided WithSeedInterval, theirs wins (last
	// option takes precedence).
	opts := append([]controller.Option{controller.WithSeedInterval(testSeedInterval)}, ctrlOpts...)

	nodeAdapter := adapter.NewNodeStoreAdapter(pgStore)
	projectAdapter := adapter.NewProjectStoreAdapter(pgStore)

	mgr := controller.NewManager(logger,
		controller.WithRequeueAfter(testRequeueAfter),
		controller.WithErrorBackoff(testErrorBackoff),
	)

	mgr.Add(controller.NewNodeHealthController(logger, nodeAdapter, bus, opts...))
	mgr.Add(controller.NewProjectSchedulerController(logger,
		projectAdapter,
		adapter.NewNodeReadyAdapter(pgStore),
		bus,
		opts...,
	))
	mgr.Add(controller.NewProjectTerminationController(logger,
		projectAdapter,
		bus,
		opts...,
	))
	mgr.Add(controller.NewProjectReschedulerController(logger,
		projectAdapter,
		nodeAdapter,
		bus,
		opts...,
	))

	return &Suite{
		Store:   pgStore,
		Manager: mgr,
		Bus:     bus,
		Pool:    pool,
		Logger:  logger,
	}
}

// Start launches the controller manager in a background goroutine.  It
// registers a t.Cleanup that cancels the context and waits briefly for the
// manager to shut down.
func (s *Suite) Start(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Manager.Start(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("Warning: controller manager did not shut down within 5s")
		}
	})

	return ctx
}

// TruncateAll removes all rows from the resources table, resetting state
// between tests.
func (s *Suite) TruncateAll(t *testing.T) {
	t.Helper()
	_, err := s.Pool.Exec(context.Background(), "TRUNCATE TABLE resources")
	require.NoError(t, err, "TruncateAll")
}

// WaitForCondition polls predicate at the given interval until it returns
// true or the timeout expires.  Fails the test on timeout.
func WaitForCondition(t *testing.T, timeout, interval time.Duration, desc string, predicate func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		if predicate() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("WaitForCondition timed out after %s: %s", timeout, desc)
		case <-tick.C:
		}
	}
}

// WaitForProjectPhase waits until the named project reaches the expected
// phase or the timeout expires.
func WaitForProjectPhase(t *testing.T, s *pgstore.Store, timeout time.Duration, projectName string, expectedPhase v1.ProjectPhase) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond,
		"project "+projectName+" phase="+string(expectedPhase),
		func() bool {
			p, err := s.GetProject(context.Background(), projectName)
			if err != nil {
				return false
			}
			return p.Status.Phase == expectedPhase
		},
	)
}

// WaitForProjectNotFound waits until the named project no longer exists in
// the store (i.e. GetProject returns store.ErrNotFound).
func WaitForProjectNotFound(t *testing.T, s *pgstore.Store, timeout time.Duration, projectName string) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond,
		"project "+projectName+" not found",
		func() bool {
			_, err := s.GetProject(context.Background(), projectName)
			return err != nil && err.Error() == store.ErrNotFound.Error()
		},
	)
}

// WaitForNodeState waits until the named node reaches the expected state
// or the timeout expires.
func WaitForNodeState(t *testing.T, s *pgstore.Store, timeout time.Duration, nodeName string, expectedState v1.NodeState) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond,
		"node "+nodeName+" state="+string(expectedState),
		func() bool {
			n, err := s.GetNode(context.Background(), nodeName)
			if err != nil {
				return false
			}
			return n.Status.State == expectedState
		},
	)
}
