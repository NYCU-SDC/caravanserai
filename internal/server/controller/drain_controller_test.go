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

// fakeDrainStore holds one Node and the Projects on it.
type fakeDrainStore struct {
	mu       sync.Mutex
	node     NodeDrainSnapshot
	projects []DrainProjectSnapshot
	owed     []string
	// pastRecall names Projects whose move CancelEviction must refuse.
	pastRecall map[string]bool

	requested []string
	cancelled []string
	statuses  []*v1.NodeDrainStatus
	writes    int
}

var _ DrainStore = (*fakeDrainStore)(nil)

func (f *fakeDrainStore) RequestEviction(_ context.Context, name, fromNode, reason string, _ v1.NodeTier) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.projects {
		p := &f.projects[i]
		if p.Name != name {
			continue
		}
		if p.Phase != v1.ProjectPhaseRunning || fromNode != f.node.Name {
			return store.ErrConflictState
		}
		if p.Eviction == nil {
			p.Eviction = &v1.ProjectEviction{Reason: reason, FromNode: fromNode, State: v1.EvictionStateQueued}
			p.EvictionFailed = nil
			f.requested = append(f.requested, name)
		}
		return nil
	}
	return store.ErrNotFound
}

func (f *fakeDrainStore) ListNodeDrains(context.Context) ([]string, error) {
	return []string{f.node.Name}, nil
}

func (f *fakeDrainStore) GetNodeDrain(_ context.Context, name string) (NodeDrainSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != f.node.Name {
		return NodeDrainSnapshot{}, store.ErrNotFound
	}
	return f.node, nil
}

func (f *fakeDrainStore) ListProjectsOnNode(context.Context, string) ([]DrainProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DrainProjectSnapshot(nil), f.projects...), nil
}

func (f *fakeDrainStore) ListProjectsOwedCleanup(context.Context, string) ([]string, error) {
	return f.owed, nil
}

func (f *fakeDrainStore) SetNodeDrainStatus(_ context.Context, _ string, status *v1.NodeDrainStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.node.Status = status
	f.statuses = append(f.statuses, status)
	f.writes++
	return nil
}

func (f *fakeDrainStore) CancelEviction(_ context.Context, name, fromNode, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pastRecall[name] {
		return false, nil
	}
	for i := range f.projects {
		p := &f.projects[i]
		if p.Name == name && p.Eviction != nil && p.Eviction.Reason == reason && p.Eviction.FromNode == fromNode {
			p.Eviction = nil
			f.cancelled = append(f.cancelled, name)
			return true, nil
		}
	}
	return false, nil
}

var drainNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func newDrainEnv(drain bool, projects ...DrainProjectSnapshot) (*fakeDrainStore, *DrainController, *fakeClock) {
	st := &fakeDrainStore{node: NodeDrainSnapshot{Name: "pve1", Drain: drain}, projects: projects}
	clock := &fakeClock{Time: drainNow}
	return st, NewDrainController(zap.NewNop(), st, nil, WithClock(clock)), clock
}

func reconcileDrain(t *testing.T, ctrl *DrainController) Result {
	t.Helper()
	res, err := ctrl.Reconcile(context.Background(), "pve1")
	require.NoError(t, err)
	return res
}

func running(name string) DrainProjectSnapshot {
	return DrainProjectSnapshot{Name: name, Phase: v1.ProjectPhaseRunning}
}

func failedMove(name, reason string, ago time.Duration) DrainProjectSnapshot {
	p := running(name)
	p.EvictionFailed = &v1.Condition{Type: v1.ConditionTypeEvictionFailed, Status: v1.ConditionTrue,
		Reason: reason, Message: "why", LastTransitionTime: drainNow.Add(-ago)}
	return p
}

func TestDrain_CordonOnlyMovesNothing(t *testing.T) {
	st, ctrl, _ := newDrainEnv(false, running("a"), running("b"))
	reconcileDrain(t, ctrl)
	assert.Empty(t, st.requested)
	assert.Zero(t, st.writes, "no drain, nothing recorded")
}

func TestDrain_AsksToMoveEveryRunningProjectOnce(t *testing.T) {
	st, ctrl, _ := newDrainEnv(true, running("a"), running("b"))
	res := reconcileDrain(t, ctrl)
	assert.ElementsMatch(t, []string{"a", "b"}, st.requested)
	assert.True(t, res.Requeue)
	require.NotNil(t, st.node.Status)
	assert.Equal(t, v1.NodeDrainDraining, st.node.Status.Phase)
	assert.Equal(t, 2, st.node.Status.Remaining)

	reconcileDrain(t, ctrl)
	assert.Len(t, st.requested, 2, "a move already in progress is not asked for again")
}

func TestDrain_Phases(t *testing.T) {
	tests := []struct {
		name         string
		projects     []DrainProjectSnapshot
		owed         []string
		wantPhase    v1.NodeDrainPhase
		wantBlocking string
	}{
		{name: "nothing left is drained", wantPhase: v1.NodeDrainDrained},
		{name: "a scheduled project will leave once running", projects: []DrainProjectSnapshot{{Name: "s", Phase: v1.ProjectPhaseScheduled}}, wantPhase: v1.NodeDrainDraining},
		{name: "a terminating project leaves on its own", projects: []DrainProjectSnapshot{{Name: "t", Phase: v1.ProjectPhaseTerminating}}, wantPhase: v1.NodeDrainDraining},
		{name: "cleanup still owed for a project that left", owed: []string{"gone"}, wantPhase: v1.NodeDrainDraining},
		{
			name:         "a failed project needs an operator",
			projects:     []DrainProjectSnapshot{{Name: "f", Phase: v1.ProjectPhaseFailed}},
			wantPhase:    v1.NodeDrainIncomplete,
			wantBlocking: "caractl takeover f",
		},
		{
			name:         "a move that cannot succeed needs an operator",
			projects:     []DrainProjectSnapshot{failedMove("pg", EvictionFailedUnsupportedBackupStrategy, time.Hour)},
			wantPhase:    v1.NodeDrainIncomplete,
			wantBlocking: "cannot be moved (UnsupportedBackupStrategy)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, ctrl, _ := newDrainEnv(true, tt.projects...)
			st.owed = tt.owed
			res := reconcileDrain(t, ctrl)
			require.NotNil(t, st.node.Status)
			assert.Equal(t, tt.wantPhase, st.node.Status.Phase)
			assert.Equal(t, tt.wantPhase != v1.NodeDrainDrained, res.Requeue)
			if tt.wantBlocking != "" {
				require.Len(t, st.node.Status.Blocking, 1)
				assert.Contains(t, st.node.Status.Blocking[0], tt.wantBlocking)
			} else {
				assert.Empty(t, st.node.Status.Blocking)
			}
		})
	}
}

func TestDrain_Retries(t *testing.T) {
	tests := []struct {
		name      string
		project   DrainProjectSnapshot
		wantRetry bool
	}{
		{name: "no room, 4m59s ago, waits", project: failedMove("a", EvictionFailedNoCapacity, 4*time.Minute+59*time.Second)},
		{name: "no room, 5m ago, retried", project: failedMove("a", EvictionFailedNoCapacity, 5*time.Minute), wantRetry: true},
		{name: "agent did not start, retried", project: failedMove("a", EvictionFailedAgentDidNotStart, time.Hour), wantRetry: true},
		{name: "not placed, retried", project: failedMove("a", EvictionFailedNotPlaced, time.Hour), wantRetry: true},
		{name: "backup failed, never retried", project: failedMove("a", EvictionFailedBackupFailed, 24*time.Hour)},
		{name: "unsupported strategy, never retried", project: failedMove("a", EvictionFailedUnsupportedBackupStrategy, 24*time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, ctrl, _ := newDrainEnv(true, tt.project)
			reconcileDrain(t, ctrl)
			assert.Equal(t, tt.wantRetry, len(st.requested) == 1)
			if tt.wantRetry {
				assert.Equal(t, v1.NodeDrainDraining, st.node.Status.Phase)
			} else {
				assert.Equal(t, v1.NodeDrainIncomplete, st.node.Status.Phase, "a project that is not leaving is listed")
			}
		})
	}
}

func TestDrain_RecordsOnlyChanges(t *testing.T) {
	st, ctrl, clock := newDrainEnv(true, DrainProjectSnapshot{Name: "s", Phase: v1.ProjectPhaseScheduled})
	reconcileDrain(t, ctrl)
	since := st.node.Status.Since
	clock.Time = clock.Time.Add(time.Minute)
	reconcileDrain(t, ctrl)
	assert.Equal(t, 1, st.writes, "an unchanged drain is not written again")

	st.projects = nil
	reconcileDrain(t, ctrl)
	assert.Equal(t, v1.NodeDrainDrained, st.node.Status.Phase)
	assert.True(t, st.node.Status.Since.After(since), "Since moves with the phase")
}

func TestDrain_CallOff(t *testing.T) {
	queued := running("queued")
	queued.Eviction = &v1.ProjectEviction{Reason: DrainReason, FromNode: "pve1", State: v1.EvictionStateQueued}
	stopped := running("stopped")
	stopped.Eviction = &v1.ProjectEviction{Reason: DrainReason, FromNode: "pve1", State: v1.EvictionStateBackingUp}
	failback := running("failback")
	failback.Eviction = &v1.ProjectEviction{Reason: "FailBack", FromNode: "pve1", State: v1.EvictionStateQueued}

	st, ctrl, _ := newDrainEnv(false, queued, stopped, failback)
	st.node.Status = &v1.NodeDrainStatus{Phase: v1.NodeDrainDraining}
	st.pastRecall = map[string]bool{"stopped": true}

	reconcileDrain(t, ctrl)
	assert.Equal(t, []string{"queued"}, st.cancelled, "only the drain's own moves that are still safe to cancel")
	assert.Nil(t, st.node.Status, "the drain's record is cleared")
	assert.Empty(t, st.requested)
}
