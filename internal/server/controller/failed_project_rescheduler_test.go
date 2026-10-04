package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeFailedProjectStore keeps Projects in memory and records the writes the
// controller makes.
type fakeFailedProjectStore struct {
	mu       sync.Mutex
	projects map[string]*ProjectSnapshot

	moves     []moveCall
	cleared   []string
	exhausted []string
}

type moveCall struct {
	Name, FromNode, Message string
}

var _ FailedProjectStore = (*fakeFailedProjectStore)(nil)

func newFakeFailedProjectStore(ps ...*ProjectSnapshot) *fakeFailedProjectStore {
	f := &fakeFailedProjectStore{projects: map[string]*ProjectSnapshot{}}
	for _, p := range ps {
		f.projects[p.Name] = p
	}
	return f
}

func (f *fakeFailedProjectStore) ListProjectNamesByPhase(_ context.Context, phase v1.ProjectPhase) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for n, p := range f.projects {
		if p.Phase == phase {
			names = append(names, n)
		}
	}
	return names, nil
}

func (f *fakeFailedProjectStore) GetProjectSnapshot(_ context.Context, name string) (*ProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakeFailedProjectStore) MoveFailedProject(_ context.Context, name, fromNode, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.projects[name]; !ok {
		return store.ErrNotFound
	}
	f.moves = append(f.moves, moveCall{name, fromNode, message})
	return nil
}

func (f *fakeFailedProjectStore) ClearFailedNodes(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, name)
	return nil
}

func (f *fakeFailedProjectStore) SetRescheduleExhausted(_ context.Context, name, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exhausted = append(f.exhausted, name)
	return nil
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// failedProject is a Project reported Failed with reason on node at t0.
func failedProject(reason, node string, failedNodes ...string) *ProjectSnapshot {
	return &ProjectSnapshot{
		Name: "app", Phase: v1.ProjectPhaseFailed, NodeRef: node, FailedNodes: failedNodes,
		Conditions: []ConditionSnapshot{{Type: v1.ConditionTypePhase, Reason: reason, LastTransitionTime: t0}},
	}
}

func runningProject(since time.Time, failedNodes ...string) *ProjectSnapshot {
	return &ProjectSnapshot{
		Name: "app", Phase: v1.ProjectPhaseRunning, NodeRef: "node-b", FailedNodes: failedNodes,
		Conditions: []ConditionSnapshot{{Type: v1.ConditionTypePhase, Reason: phaseReasonRunning, LastTransitionTime: since}},
	}
}

// reconcileAt runs one Reconcile of "app" with the clock set to t0 + after.
func reconcileAt(t *testing.T, st *fakeFailedProjectStore, after time.Duration) Result {
	t.Helper()
	clock := &fakeClock{Time: t0.Add(after)}
	ctrl := NewFailedProjectReschedulerController(zap.NewNop(), st, nil, WithClock(clock))
	res, err := ctrl.Reconcile(context.Background(), "app")
	require.NoError(t, err)
	return res
}

func TestFailedProjectRescheduler_MoveTiming(t *testing.T) {
	tests := []struct {
		name     string
		project  *ProjectSnapshot
		after    time.Duration
		wantMove bool
		wantWait bool
	}{
		{name: "first move waits out one minute", project: failedProject("ContainerExited", "node-a"), after: 59 * time.Second, wantWait: true},
		{name: "first move happens after one minute", project: failedProject("ContainerExited", "node-a"), after: time.Minute, wantMove: true},
		{name: "second move waits out five minutes", project: failedProject("ContainerExited", "node-b", "node-a"), after: 4*time.Minute + 59*time.Second, wantWait: true},
		{name: "second move happens after five minutes", project: failedProject("ContainerExited", "node-b", "node-a"), after: 5 * time.Minute, wantMove: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeFailedProjectStore(tt.project)
			res := reconcileAt(t, st, tt.after)
			assert.Equal(t, tt.wantWait, res.Requeue, "a project still in backoff is checked again")
			if tt.wantMove {
				require.Len(t, st.moves, 1)
				assert.Equal(t, tt.project.NodeRef, st.moves[0].FromNode, "the node it leaves is the one it failed on")
				assert.Contains(t, st.moves[0].Message, tt.project.NodeRef)
			} else {
				assert.Empty(t, st.moves)
			}
		})
	}
}

func TestFailedProjectRescheduler_Reasons(t *testing.T) {
	tests := []struct {
		reason   string
		wantMove bool
	}{
		{"LocalRestartExhausted", true},
		{"ContainerRestartStuck", true},
		{"InspectError", true},
		{"ReconcileError", true},
		{"ContainerExited", true},
		{"ContainerCrashed", true},
		{"RemoveError", false},
		{"SecretNotFound", false},
		{"SecretKeyNotFound", false},
		{"RestoreError", false},
		{"ContainerUnhealthy", false},
		{"ContainerMissing", false},
		{"SomethingAddedLater", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			st := newFakeFailedProjectStore(failedProject(tt.reason, "node-a"))
			res := reconcileAt(t, st, time.Hour)
			assert.False(t, res.Requeue)
			assert.Equal(t, tt.wantMove, len(st.moves) == 1)
			assert.Empty(t, st.exhausted)
		})
	}
}

func TestFailedProjectRescheduler_Exhausted(t *testing.T) {
	t.Run("out of moves raises the alert and does not move", func(t *testing.T) {
		st := newFakeFailedProjectStore(failedProject("LocalRestartExhausted", "node-c", "node-a", "node-b"))
		res := reconcileAt(t, st, 24*time.Hour)
		assert.False(t, res.Requeue)
		assert.Empty(t, st.moves)
		assert.Equal(t, []string{"app"}, st.exhausted)
	})

	t.Run("the alert is raised once", func(t *testing.T) {
		p := failedProject("LocalRestartExhausted", "node-c", "node-a", "node-b")
		p.Conditions = append(p.Conditions, ConditionSnapshot{Type: v1.ConditionTypeRescheduleExhausted})
		st := newFakeFailedProjectStore(p)
		reconcileAt(t, st, 24*time.Hour)
		reconcileAt(t, st, 48*time.Hour)
		assert.Empty(t, st.exhausted, "a project already marked exhausted is not announced again")
		assert.Empty(t, st.moves)
	})

	t.Run("a reason that is not moved is not announced as exhausted either", func(t *testing.T) {
		st := newFakeFailedProjectStore(failedProject("SecretNotFound", "node-c", "node-a", "node-b"))
		reconcileAt(t, st, 24*time.Hour)
		assert.Empty(t, st.exhausted)
	})
}

func TestFailedProjectRescheduler_Stability(t *testing.T) {
	tests := []struct {
		name      string
		project   *ProjectSnapshot
		after     time.Duration
		wantClear bool
	}{
		{name: "not yet stable", project: runningProject(t0, "node-a"), after: 3*time.Hour - time.Second},
		{name: "stable after three hours clears the history", project: runningProject(t0, "node-a"), after: 3 * time.Hour, wantClear: true},
		{name: "a project that never moved has nothing to clear", project: runningProject(t0), after: 10 * time.Hour},
		{
			name: "without the time it began running it is not judged stable",
			project: func() *ProjectSnapshot {
				p := runningProject(t0, "node-a")
				p.Conditions[0].Reason = "RescheduledAfterFailure"
				return p
			}(),
			after: 10 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeFailedProjectStore(tt.project)
			res := reconcileAt(t, st, tt.after)
			assert.False(t, res.Requeue)
			assert.Equal(t, tt.wantClear, len(st.cleared) == 1)
			assert.Empty(t, st.moves)
		})
	}
}

func TestFailedProjectRescheduler_NoOps(t *testing.T) {
	t.Run("project not found", func(t *testing.T) {
		st := newFakeFailedProjectStore()
		res := reconcileAt(t, st, time.Hour)
		assert.False(t, res.Requeue)
	})

	t.Run("pending, scheduled and terminating projects are left alone", func(t *testing.T) {
		for _, phase := range []v1.ProjectPhase{v1.ProjectPhasePending, v1.ProjectPhaseScheduled, v1.ProjectPhaseTerminating} {
			p := failedProject("ContainerExited", "node-a")
			p.Phase = phase
			st := newFakeFailedProjectStore(p)
			reconcileAt(t, st, time.Hour)
			assert.Empty(t, st.moves, "phase %s", phase)
		}
	})

	t.Run("a failed project with no node has nothing to move off", func(t *testing.T) {
		st := newFakeFailedProjectStore(failedProject("ContainerExited", ""))
		reconcileAt(t, st, time.Hour)
		assert.Empty(t, st.moves)
	})

	t.Run("failed with no phase condition is left alone", func(t *testing.T) {
		p := failedProject("ContainerExited", "node-a")
		p.Conditions = nil
		st := newFakeFailedProjectStore(p)
		reconcileAt(t, st, time.Hour)
		assert.Empty(t, st.moves)
	})
}

func TestRescheduleBudget(t *testing.T) {
	assert.Equal(t, 2, maxReschedules, "the retry cap is two moves")
	assert.Equal(t, []time.Duration{time.Minute, 5 * time.Minute}, rescheduleBackoff[:])
	assert.Equal(t, 3*time.Hour, rescheduleStableAfter)
}

func TestFailedProjectRescheduler_Resync(t *testing.T) {
	st := newFakeFailedProjectStore(
		failedProject("ContainerExited", "node-a"),
		&ProjectSnapshot{Name: "up", Phase: v1.ProjectPhaseRunning},
		&ProjectSnapshot{Name: "waiting", Phase: v1.ProjectPhasePending},
	)
	ctrl := NewFailedProjectReschedulerController(zap.NewNop(), st, nil)

	var got []string
	ctrl.resync(context.Background(), zap.NewNop(), func(n string) { got = append(got, n) })
	assert.ElementsMatch(t, []string{"app", "up"}, got, "Failed and Running projects are watched, Pending ones are not")
}
