package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeClock is a manually advanced clock, mirroring the server controllers'
// test clock so grace periods can be crossed without sleeping.
type fakeClock struct {
	now time.Time
}

func (fc *fakeClock) Now() time.Time                  { return fc.now }
func (fc *fakeClock) Since(t time.Time) time.Duration { return fc.now.Sub(t) }
func (fc *fakeClock) advance(d time.Duration)         { fc.now = fc.now.Add(d) }

// recordingRoutes records Remove calls so route cleanup can be asserted.
type recordingRoutes struct {
	mu      sync.Mutex
	removed []string
}

func (r *recordingRoutes) Update(*v1.Project, map[string]string) {}

func (r *recordingRoutes) Remove(projectName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, projectName)
}

func (r *recordingRoutes) removedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func projectsNamed(names ...string) []*v1.Project {
	out := make([]*v1.Project, 0, len(names))
	for _, n := range names {
		out = append(out, &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: n, Namespace: "default"},
		})
	}
	return out
}

// newFailingListServer serves a 500 for the reconcile listing, standing in for
// an unreachable or erroring control plane.
func newFailingListServer(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	return server, NewClient(zap.NewNop(), server.URL, "test-node")
}

// newSweepFixture wires a mockRuntime whose local containers are localNames and
// records which projects the sweep removed.
func newSweepFixture(localNames []string) (*mockRuntime, *[]string) {
	var removed []string
	rt := &mockRuntime{
		listLocalFn: func(context.Context) ([]string, error) {
			return append([]string(nil), localNames...), nil
		},
		removeOrphanFn: func(_ context.Context, name string) error {
			removed = append(removed, name)
			return nil
		},
	}
	return rt, &removed
}

func TestSweepOrphans_RemovesAfterGracePeriod(t *testing.T) {
	rt, removed := newSweepFixture([]string{"gone", "kept"})
	routes := &recordingRoutes{}
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	assigned := projectsNamed("kept")

	// First observation starts the clock but must not remove anything.
	sweepOrphans(context.Background(), rt, routes, tracker, assigned, zap.NewNop())
	assert.Empty(t, *removed, "first observation must not remove")
	assert.Equal(t, 1, tracker.pending())

	// Still inside the grace period.
	fc.advance(orphanGracePeriod - time.Second)
	sweepOrphans(context.Background(), rt, routes, tracker, assigned, zap.NewNop())
	assert.Empty(t, *removed, "must not remove before the grace period elapses")

	// Grace period reached.
	fc.advance(2 * time.Second)
	sweepOrphans(context.Background(), rt, routes, tracker, assigned, zap.NewNop())

	require.Equal(t, []string{"gone"}, *removed)
	assert.Equal(t, []string{"gone"}, routes.removedNames())
	assert.Equal(t, 0, tracker.pending(), "removed project must stop being tracked")
}

func TestSweepOrphans_ReappearedProjectIsSpared(t *testing.T) {
	rt, removed := newSweepFixture([]string{"flapping"})
	routes := &recordingRoutes{}
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	// Absent: starts the clock.
	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())
	require.Equal(t, 1, tracker.pending())

	// Reappears before the grace period elapses — the candidacy is dropped.
	fc.advance(orphanGracePeriod - time.Second)
	sweepOrphans(context.Background(), rt, routes, tracker, projectsNamed("flapping"), zap.NewNop())
	assert.Equal(t, 0, tracker.pending(), "reappearing project must clear its clock")

	// Absent again, past what would have been the original deadline. Because
	// the clock restarted, this must still not remove.
	fc.advance(2 * time.Second)
	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())
	assert.Empty(t, *removed, "an intermittent absence must not accumulate toward removal")
	assert.Equal(t, 1, tracker.pending())
}

func TestSweepOrphans_NoOrphansIsNoOp(t *testing.T) {
	rt, removed := newSweepFixture([]string{"a", "b"})
	routes := &recordingRoutes{}
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	assigned := projectsNamed("a", "b")

	sweepOrphans(context.Background(), rt, routes, tracker, assigned, zap.NewNop())
	fc.advance(2 * orphanGracePeriod)
	sweepOrphans(context.Background(), rt, routes, tracker, assigned, zap.NewNop())

	assert.Empty(t, *removed)
	assert.Empty(t, routes.removedNames())
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_ListErrorIsNoOp(t *testing.T) {
	var removed []string
	rt := &mockRuntime{
		listLocalFn: func(context.Context) ([]string, error) {
			return nil, errors.New("docker unavailable")
		},
		removeOrphanFn: func(_ context.Context, name string) error {
			removed = append(removed, name)
			return nil
		},
	}
	routes := &recordingRoutes{}
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())
	fc.advance(2 * orphanGracePeriod)
	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())

	assert.Empty(t, removed, "a failed local listing must never remove anything")
	assert.Equal(t, 0, tracker.pending(), "a failed listing must not start any clock")
}

// A failed ListProjectsForReconcile must leave the sweep untouched: no removal,
// and no clock advanced. This exercises the reconcileProjects error path rather
// than sweepOrphans directly.
func TestReconcileProjects_ListErrorSkipsSweep(t *testing.T) {
	srv, client := newFailingListServer(t)
	defer srv.Close()

	rt, removed := newSweepFixture([]string{"orphan"})
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	reconcileProjects(context.Background(), client, rt, nil, nil, tracker, zap.NewNop())
	fc.advance(2 * orphanGracePeriod)
	reconcileProjects(context.Background(), client, rt, nil, nil, tracker, zap.NewNop())

	assert.Empty(t, *removed, "an unreachable server must not be read as everything being orphaned")
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_RemoveFailureKeepsCandidate(t *testing.T) {
	var attempts int
	rt := &mockRuntime{
		listLocalFn: func(context.Context) ([]string, error) {
			return []string{"stubborn"}, nil
		},
		removeOrphanFn: func(context.Context, string) error {
			attempts++
			return errors.New("docker refused")
		},
	}
	routes := &recordingRoutes{}
	fc := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(fc)

	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())
	fc.advance(orphanGracePeriod)
	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())
	sweepOrphans(context.Background(), rt, routes, tracker, nil, zap.NewNop())

	assert.Equal(t, 2, attempts, "a failed removal must be retried on the next tick")
	assert.Empty(t, routes.removedNames(), "routes must survive a failed container removal")
	assert.Equal(t, 1, tracker.pending())
}
