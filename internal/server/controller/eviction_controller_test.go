package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeEvictionProject is one Project in fakeEvictionStore.
type fakeEvictionProject struct {
	Phase   v1.ProjectPhase
	NodeRef string
	Size    v1.ProjectSize
	Managed bool
	PgDiff  bool
	// Stopped is the Agent's workload-stopped evidence for the current
	// assignment, which the release needs.
	Stopped bool
	// Excluded are the Nodes the scheduler will not place it on.
	Excluded   []string
	Eviction   *v1.ProjectEviction
	Conditions []v1.Condition
	Failure    *EvictionFailure
}

// fakeEvictionStore mirrors the adapter's guards closely enough to drive the
// controller through whole moves, and serves the capacity ledger from the
// same Projects.
type fakeEvictionStore struct {
	mu              sync.Mutex
	lastMoveMessage string
	clock           *fakeClock
	projects        map[string]*fakeEvictionProject

	backupsAsked []string
	requests     []string
	released     []string
}

func newFakeEvictionStore(clock *fakeClock) *fakeEvictionStore {
	return &fakeEvictionStore{clock: clock, projects: map[string]*fakeEvictionProject{}}
}

// running adds a Running Project on node.
func (f *fakeEvictionStore) running(name, node string, size v1.ProjectSize, managed bool) {
	f.projects[name] = &fakeEvictionProject{Phase: v1.ProjectPhaseRunning, NodeRef: node, Size: size, Managed: managed}
}

// request queues a move as RequestEviction would, after skew.
func (f *fakeEvictionStore) request(name, reason string, skew time.Duration) {
	p := f.projects[name]
	at := f.clock.Now().Add(skew)
	p.Eviction = &v1.ProjectEviction{Reason: reason, FromNode: p.NodeRef, State: v1.EvictionStateQueued, RequestedAt: at, StateSince: at}
}

// agentReports writes the agent's backup result now.
func (f *fakeEvictionStore) agentReports(name string, status v1.ConditionStatus, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	// The Agent records the stop evidence before it reports success.
	if status == v1.ConditionTrue {
		p.Stopped = true
	}
	p.Conditions = append(p.Conditions, v1.Condition{
		Type: v1.ConditionTypeEvictionBackup, Status: status, Message: message, LastTransitionTime: f.clock.Now(),
	})
}

// agentConfirmsStopped records only the stop evidence, as when the Agent's
// result report is lost.
func (f *fakeEvictionStore) agentConfirmsStopped(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects[name].Stopped = true
}

// schedule places a Pending Project as the scheduler would.
func (f *fakeEvictionStore) schedule(name, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	p.Phase, p.NodeRef = v1.ProjectPhaseScheduled, node
}

func (f *fakeEvictionStore) get(name string) fakeEvictionProject {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.projects[name]
}

func (f *fakeEvictionStore) ListProjectsByNodeRef(_ context.Context, node string, phases []v1.ProjectPhase) ([]*ProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ProjectSnapshot
	for n, p := range f.projects {
		if p.NodeRef == node && slices.Contains(phases, p.Phase) {
			conditions := make([]ConditionSnapshot, len(p.Conditions))
			for i, c := range p.Conditions {
				conditions[i] = ConditionSnapshot{Type: c.Type, Reason: c.Reason, LastTransitionTime: c.LastTransitionTime}
			}
			out = append(out, &ProjectSnapshot{
				Name: n, Phase: p.Phase, NodeRef: p.NodeRef, Size: p.Size,
				ExcludedNodes: slices.Clone(p.Excluded), Evicting: p.Eviction != nil, Conditions: conditions,
			})
		}
	}
	return out, nil
}

func (f *fakeEvictionStore) ListEvictions(context.Context) ([]*EvictionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*EvictionSnapshot
	for n, p := range f.projects {
		if p.Eviction == nil {
			continue
		}
		out = append(out, &EvictionSnapshot{
			Name: n, Phase: p.Phase, NodeRef: p.NodeRef, Size: p.Size, HasManagedVolumes: p.Managed,
			HasPostgresDifferential: p.PgDiff, WorkloadStopped: p.Stopped, ExcludedNodes: slices.Clone(p.Excluded),
			Eviction: *p.Eviction, Conditions: slices.Clone(p.Conditions),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Eviction.RequestedAt.Before(out[j].Eviction.RequestedAt) })
	return out, nil
}

func (f *fakeEvictionStore) StartEvictionBackup(_ context.Context, name, fromNode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	if p.Eviction == nil || p.Eviction.State != v1.EvictionStateQueued || p.NodeRef != fromNode {
		return nil
	}
	p.Eviction.State, p.Eviction.StateSince = v1.EvictionStateBackingUp, f.clock.Now()
	p.Conditions = v1.RemoveConditions(p.Conditions, v1.ConditionTypeEvictionBackup)
	f.backupsAsked = append(f.backupsAsked, name)
	return nil
}

// MoveEvictedProject mirrors the adapter's release gate: no stop evidence,
// no release.
func (f *fakeEvictionStore) MoveEvictedProject(_ context.Context, e *EvictionSnapshot, message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMoveMessage = message
	p := f.projects[e.Name]
	if p.Eviction == nil || p.Phase != v1.ProjectPhaseRunning || p.NodeRef != e.NodeRef || !p.Stopped {
		return false, nil
	}
	p.Eviction.State, p.Eviction.StateSince = v1.EvictionStateMoving, f.clock.Now()
	p.Phase, p.NodeRef = v1.ProjectPhasePending, ""
	p.Stopped = false
	f.released = append(f.released, e.Name)
	return true, nil
}

// RequestEviction mirrors the adapter's guards.
func (f *fakeEvictionStore) CancelQueuedEviction(_ context.Context, name, fromNode, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	if p == nil || p.Eviction == nil || p.Eviction.State != v1.EvictionStateQueued ||
		p.Eviction.Reason != reason || p.Eviction.FromNode != fromNode {
		return false, nil
	}
	p.Eviction = nil
	return true, nil
}

func (f *fakeEvictionStore) RequestEviction(_ context.Context, name, fromNode, reason string, tier v1.NodeTier) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[name]
	if !ok {
		return store.ErrNotFound
	}
	if p.Eviction != nil {
		return nil
	}
	if p.Phase != v1.ProjectPhaseRunning || p.NodeRef != fromNode {
		return store.ErrConflictState
	}
	now := f.clock.Now()
	p.Eviction = &v1.ProjectEviction{Reason: reason, FromNode: fromNode, State: v1.EvictionStateQueued, TargetTier: tier, RequestedAt: now, StateSince: now}
	f.requests = append(f.requests, name)
	return nil
}

// EndEviction mirrors the adapter: a move whose stop evidence is in is not
// abandoned.
func (f *fakeEvictionStore) EndEviction(_ context.Context, e *EvictionSnapshot, failure *EvictionFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[e.Name]
	if p.Eviction == nil {
		return nil
	}
	if failure != nil && p.Phase == v1.ProjectPhaseRunning && p.Stopped {
		return fmt.Errorf("%w: reported stopped", store.ErrConflictState)
	}
	p.Eviction = nil
	p.Failure = failure
	return nil
}

func (f *fakeEvictionStore) MarkEvictionAgentStarted(_ context.Context, e *EvictionSnapshot, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[e.Name]
	if p.Eviction == nil || p.Eviction.State != v1.EvictionStateBackingUp || !p.Eviction.StateSince.Equal(e.Eviction.StateSince) || p.Eviction.AgentStartedAt != nil {
		return nil
	}
	p.Eviction.AgentStartedAt = &at
	return nil
}

// evictionEnv is a controller over a fake store and a set of 4c / 8Gi nodes.
type evictionEnv struct {
	clock *fakeClock
	store *fakeEvictionStore
	ctrl  *EvictionController
}

func newEvictionEnv(t *testing.T, maxInFlight int, nodes ...string) *evictionEnv {
	t.Helper()
	clock := &fakeClock{Time: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	st := newFakeEvictionStore(clock)
	ready := make([]ReadyNode, len(nodes))
	for i, n := range nodes {
		ready[i] = ReadyNode{Name: n, Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}}
	}
	ctrl := NewEvictionController(zap.NewNop(), st, newFakeSchedulerNodeStoreOf(ready...), nil, maxInFlight, WithClock(clock))
	return &evictionEnv{clock: clock, store: st, ctrl: ctrl}
}

// newTieredEvictionEnv is newEvictionEnv with each node labelled with the
// tier given in tiers, by name.
func newTieredEvictionEnv(t *testing.T, tiers map[string]string) *evictionEnv {
	t.Helper()
	clock := &fakeClock{Time: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	st := newFakeEvictionStore(clock)
	var ready []ReadyNode
	for _, n := range []string{"node-a", "node-b", "node-c"} {
		tier, ok := tiers[n]
		if !ok {
			continue
		}
		ready = append(ready, ReadyNode{
			Name: n, Labels: map[string]string{v1.LabelNodeTier: tier},
			Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"},
		})
	}
	ctrl := NewEvictionController(zap.NewNop(), st, newFakeSchedulerNodeStoreOf(ready...), nil, 1, WithClock(clock))
	return &evictionEnv{clock: clock, store: st, ctrl: ctrl}
}

func (e *evictionEnv) reconcile(t *testing.T) {
	t.Helper()
	_, err := e.ctrl.Reconcile(context.Background(), evictionQueueKey)
	require.NoError(t, err)
}

func (e *evictionEnv) advance(d time.Duration) { e.clock.Time = e.clock.Time.Add(d) }

func stateOf(p fakeEvictionProject) v1.EvictionState {
	if p.Eviction == nil {
		return ""
	}
	return p.Eviction.State
}

func TestEvictionController_SingleMove(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("app", "node-a", v1.ProjectSizeMedium, true)
	env.store.request("app", "Drain", 0)

	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")), "the agent is asked for a fresh backup")
	assert.Equal(t, v1.ProjectPhaseRunning, env.store.get("app").Phase, "still running until the backup is in")

	env.advance(time.Minute)
	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")), "waiting on the agent")

	env.store.agentReports("app", v1.ConditionTrue, "backup-1")
	env.reconcile(t)
	p := env.store.get("app")
	assert.Equal(t, v1.EvictionStateMoving, stateOf(p))
	assert.Equal(t, v1.ProjectPhasePending, p.Phase)
	assert.Empty(t, p.NodeRef)

	env.store.schedule("app", "node-b")
	env.reconcile(t)
	p = env.store.get("app")
	assert.Nil(t, p.Eviction, "the move is done once the project is placed")
	assert.Nil(t, p.Failure)
}

func TestEvictionController_ProjectWithoutDataIsStoppedButNotBackedUp(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("app", "node-a", v1.ProjectSizeSmall, false)
	env.store.request("app", "FailBack", 0)

	// No data to carry, but the release gate still needs the Agent to stop
	// it, so the old copy is not running beside the new one.
	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	assert.Equal(t, v1.ProjectPhaseRunning, env.store.get("app").Phase)

	env.store.agentReports("app", v1.ConditionTrue, "")
	env.reconcile(t)
	p := env.store.get("app")
	assert.Equal(t, v1.EvictionStateMoving, stateOf(p))
	assert.Equal(t, v1.ProjectPhasePending, p.Phase)
}

func TestEvictionController_MaxInFlight(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("first", "node-a", v1.ProjectSizeSmall, true)
	env.store.running("second", "node-a", v1.ProjectSizeSmall, true)
	env.store.request("second", "Drain", time.Second)
	env.store.request("first", "Drain", 0)

	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("first")), "oldest request first")
	assert.Equal(t, v1.EvictionStateQueued, stateOf(env.store.get("second")), "one move at a time")

	env.store.agentReports("first", v1.ConditionTrue, "b1")
	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateMoving, stateOf(env.store.get("first")))
	assert.Equal(t, v1.EvictionStateQueued, stateOf(env.store.get("second")), "the slot is held until the first is placed")

	env.store.schedule("first", "node-b")
	env.reconcile(t)
	assert.Nil(t, env.store.get("first").Eviction)
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("second")), "the next move starts once the slot frees")
}

func TestEvictionController_MaxInFlightIsConfigurable(t *testing.T) {
	env := newEvictionEnv(t, 2, "node-a", "node-b")
	for _, n := range []string{"a", "b", "c"} {
		env.store.running(n, "node-a", v1.ProjectSizeSmall, true)
	}
	env.store.request("a", "Drain", 0)
	env.store.request("b", "Drain", time.Second)
	env.store.request("c", "Drain", 2*time.Second)

	env.reconcile(t)
	assert.Equal(t, []string{"a", "b"}, env.store.backupsAsked)
	assert.Equal(t, v1.EvictionStateQueued, stateOf(env.store.get("c")))
}

func TestEvictionController_HeadroomReevaluatedBetweenMoves(t *testing.T) {
	// node-b has room for one Large project (2c / 4Gi of 4c / 8Gi) beside the
	// Large one already there, not two.
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("resident", "node-b", v1.ProjectSizeLarge, false)
	env.store.running("first", "node-a", v1.ProjectSizeLarge, true)
	env.store.running("second", "node-a", v1.ProjectSizeLarge, true)
	env.store.request("first", "Drain", 0)
	env.store.request("second", "Drain", time.Second)

	env.reconcile(t)
	env.store.agentReports("first", v1.ConditionTrue, "b1")
	env.reconcile(t)
	env.store.schedule("first", "node-b")
	env.store.mu.Lock()
	env.store.projects["first"].Phase = v1.ProjectPhaseRunning
	env.store.mu.Unlock()
	env.reconcile(t)

	second := env.store.get("second")
	assert.Nil(t, second.Eviction, "the second move's precheck sees node-b already taken by the first")
	require.NotNil(t, second.Failure)
	assert.Equal(t, EvictionFailedNoCapacity, second.Failure.Reason)
	assert.Equal(t, v1.ProjectPhaseRunning, second.Phase, "a refused move leaves the project where it is")
	assert.Equal(t, []string{"first"}, env.store.backupsAsked, "no backup was asked for the refused move")
}

func TestEvictionController_Failures(t *testing.T) {
	tests := []struct {
		name        string
		nodes       []string
		setup       func(env *evictionEnv)
		wantReason  string
		wantRunning bool
	}{
		{
			name:  "no other node can take it",
			nodes: []string{"node-a"},
			setup: func(env *evictionEnv) {
				env.reconcile(t)
			},
			wantReason: EvictionFailedNoCapacity, wantRunning: true,
		},
		{
			// The scheduler would not place it there, so stopping it for a
			// move it could not finish would be downtime for nothing.
			name:  "the only other node still owes the project cleanup",
			nodes: []string{"node-a", "node-b"},
			setup: func(env *evictionEnv) {
				env.store.projects["app"].Excluded = []string{"node-b"}
				env.reconcile(t)
			},
			wantReason: EvictionFailedNoCapacity, wantRunning: true,
		},
		{
			name:  "the backup failed",
			nodes: []string{"node-a", "node-b"},
			setup: func(env *evictionEnv) {
				env.reconcile(t)
				env.store.agentReports("app", v1.ConditionFalse, "object store unreachable")
				env.reconcile(t)
			},
			wantReason: EvictionFailedBackupFailed, wantRunning: true,
		},
		{
			name:  "the agent never started the backup",
			nodes: []string{"node-a", "node-b"},
			setup: func(env *evictionEnv) {
				env.reconcile(t)
				env.advance(evictionStartTimeout)
				env.reconcile(t)
			},
			wantReason: EvictionFailedAgentDidNotStart, wantRunning: true,
		},
		{
			name:  "no node took the project after it left",
			nodes: []string{"node-a", "node-b"},
			setup: func(env *evictionEnv) {
				env.reconcile(t)
				env.store.agentReports("app", v1.ConditionTrue, "b1")
				env.reconcile(t)
				env.advance(evictionPlaceTimeout)
				env.reconcile(t)
			},
			wantReason: EvictionFailedNotPlaced,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEvictionEnv(t, 1, tt.nodes...)
			env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
			env.store.request("app", "Drain", 0)
			tt.setup(env)

			p := env.store.get("app")
			assert.Nil(t, p.Eviction)
			require.NotNil(t, p.Failure)
			assert.Equal(t, tt.wantReason, p.Failure.Reason)
			assert.NotEmpty(t, p.Failure.Message)
			assert.Equal(t, tt.wantRunning, p.Phase == v1.ProjectPhaseRunning)
		})
	}
}

func TestEvictionController_Waits(t *testing.T) {
	t.Run("a backup in progress is not timed out", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)
		env.advance(evictionStartTimeout - time.Minute)
		env.store.mu.Lock()
		env.store.projects["app"].Conditions = []v1.Condition{{
			Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, LastTransitionTime: env.clock.Now(),
		}}
		env.store.mu.Unlock()
		env.advance(2 * time.Minute)
		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})

	t.Run("not yet timed out", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)
		env.advance(evictionStartTimeout - time.Second)
		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})

	t.Run("a result from before this move is ignored", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.advance(time.Second)
		env.reconcile(t)
		// StartEvictionBackup cleared it; write a stale one back by hand.
		env.store.mu.Lock()
		env.store.projects["app"].Conditions = []v1.Condition{{
			Type: v1.ConditionTypeEvictionBackup, Status: v1.ConditionTrue, LastTransitionTime: env.clock.Now().Add(-time.Hour),
		}}
		env.store.mu.Unlock()
		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})
}

func TestEvictionController_DropsMovesOverTakenByEvents(t *testing.T) {
	tests := []struct {
		name   string
		before func(env *evictionEnv)
		change func(p *fakeEvictionProject)
	}{
		{
			name:   "queued, then failed over",
			change: func(p *fakeEvictionProject) { p.Phase, p.NodeRef = v1.ProjectPhasePending, "" },
		},
		{
			name:   "backing up, then the node went NotReady and the project was moved",
			before: func(env *evictionEnv) { env.reconcile(t) },
			change: func(p *fakeEvictionProject) { p.Phase, p.NodeRef = v1.ProjectPhaseScheduled, "node-b" },
		},
		{
			name:   "backing up, then failed",
			before: func(env *evictionEnv) { env.reconcile(t) },
			change: func(p *fakeEvictionProject) { p.Phase = v1.ProjectPhaseFailed },
		},
		{
			name: "moving, then deleted",
			before: func(env *evictionEnv) {
				env.reconcile(t)
				env.store.agentReports("app", v1.ConditionTrue, "b1")
				env.reconcile(t)
			},
			change: func(p *fakeEvictionProject) { p.Phase = v1.ProjectPhaseTerminating },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newEvictionEnv(t, 1, "node-a", "node-b")
			env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
			env.store.request("app", "Drain", 0)
			if tt.before != nil {
				tt.before(env)
			}
			env.store.mu.Lock()
			tt.change(env.store.projects["app"])
			env.store.mu.Unlock()
			env.reconcile(t)

			p := env.store.get("app")
			assert.Nil(t, p.Eviction)
			assert.Nil(t, p.Failure, "not a failure of the move")
		})
	}
}

func TestEvictionController_Defaults(t *testing.T) {
	ctrl := NewEvictionController(zap.NewNop(), newFakeEvictionStore(&fakeClock{}), newFakeSchedulerNodeStore(), nil, 0)
	assert.Equal(t, 1, ctrl.maxInFlight, "below one means one")
	assert.Equal(t, 1, DefaultMaxInFlightEvictions)
	assert.Equal(t, 10*time.Minute, evictionStartTimeout)
	assert.Equal(t, 10*time.Minute, evictionPlaceTimeout)

	res, err := ctrl.Reconcile(context.Background(), evictionQueueKey)
	require.NoError(t, err)
	assert.False(t, res.Requeue, "nothing to do, nothing to come back for")
}

func TestEvictionController_TargetTier(t *testing.T) {
	t.Run("a move bound for primary is refused when only backup has room", func(t *testing.T) {
		env := newTieredEvictionEnv(t, map[string]string{"node-a": "backup", "node-b": "backup"})
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "FailBack", 0)
		env.store.projects["app"].Eviction.TargetTier = v1.NodeTierPrimary

		env.reconcile(t)
		p := env.store.get("app")
		assert.Nil(t, p.Eviction)
		require.NotNil(t, p.Failure)
		assert.Equal(t, EvictionFailedNoCapacity, p.Failure.Reason)
		assert.Contains(t, p.Failure.Message, "No other primary node")
		assert.Empty(t, env.store.backupsAsked, "nothing is stopped for a move with nowhere to go")
	})

	t.Run("a move bound for primary starts when primary has room", func(t *testing.T) {
		env := newTieredEvictionEnv(t, map[string]string{"node-a": "backup", "node-b": "primary"})
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "FailBack", 0)
		env.store.projects["app"].Eviction.TargetTier = v1.NodeTierPrimary

		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})

	t.Run("a move with no target tier may land in either tier", func(t *testing.T) {
		env := newTieredEvictionEnv(t, map[string]string{"node-a": "primary", "node-b": "backup"})
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)

		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})
}

func TestEvictionController_ReleaseNeedsStopEvidence(t *testing.T) {
	t.Run("a success report without the evidence does not release", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)

		// What the gate rests on is the evidence, not the report.
		env.store.mu.Lock()
		env.store.projects["app"].Conditions = append(env.store.projects["app"].Conditions, v1.Condition{
			Type: v1.ConditionTypeEvictionBackup, Status: v1.ConditionTrue, LastTransitionTime: env.clock.Now(),
		})
		env.store.mu.Unlock()
		env.reconcile(t)

		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
		assert.Empty(t, env.store.released)
	})

	t.Run("the evidence releases even when the report was lost", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)

		env.store.agentConfirmsStopped("app")
		env.reconcile(t)
		assert.Equal(t, []string{"app"}, env.store.released)
		assert.Equal(t, v1.EvictionStateMoving, stateOf(env.store.get("app")))
	})

	t.Run("once stopped the move is not timed out", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)
		env.store.agentConfirmsStopped("app")
		env.advance(evictionStartTimeout + time.Minute)
		env.reconcile(t)
		assert.Nil(t, env.store.get("app").Failure, "a stopped project is moved, never abandoned")
	})
}

func TestEvictionController_PostgresDifferentialIsRefused(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
	env.store.projects["app"].PgDiff = true
	env.store.request("app", "Drain", 0)

	env.reconcile(t)
	p := env.store.get("app")
	assert.Nil(t, p.Eviction)
	require.NotNil(t, p.Failure)
	assert.Equal(t, EvictionFailedUnsupportedBackupStrategy, p.Failure.Reason)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Phase, "left running where it is")
	assert.Empty(t, env.store.backupsAsked, "nothing is stopped")
}

// ── Review A1: a move abandoned while its agent was finishing ────────────────

func (e *evictionEnv) setConditions(name string, conds ...v1.Condition) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	e.store.projects[name].Conditions = conds
}

func maintenanceAt(at time.Time) v1.Condition {
	return v1.Condition{Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, LastTransitionTime: at}
}

func TestEvictionController_AStartedBackupIsNotTimedOut(t *testing.T) {
	t.Run("the agent clears Maintenance before it reports", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t) // BackingUp
		env.advance(time.Minute)
		env.setConditions("app", maintenanceAt(env.clock.Now()))
		env.reconcile(t)
		require.NotNil(t, env.store.get("app").Eviction.AgentStartedAt, "seen at work once is remembered")

		// The backup took 12 minutes; Maintenance is gone while old backups
		// are pruned, and no result or evidence has arrived yet.
		env.advance(11 * time.Minute)
		env.setConditions("app")
		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")), "not taken for an agent that never started")
	})

	t.Run("a backup running past MaintenanceStaleAfter", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)
		env.setConditions("app", maintenanceAt(env.clock.Now()))
		env.reconcile(t)
		env.advance(v1.MaintenanceStaleAfter + time.Minute)
		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})

	t.Run("an agent that went silent is given up on in the end", func(t *testing.T) {
		env := newEvictionEnv(t, 1, "node-a", "node-b")
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "Drain", 0)
		env.reconcile(t)
		env.setConditions("app", maintenanceAt(env.clock.Now()))
		env.reconcile(t)
		env.setConditions("app")
		env.advance(evictionBackupTimeout)
		env.reconcile(t)
		p := env.store.get("app")
		assert.Nil(t, p.Eviction)
		require.NotNil(t, p.Failure)
		assert.Equal(t, EvictionFailedAgentDidNotFinish, p.Failure.Reason)
	})
}

// The decision to abandon was made on a read from before the evidence came
// in. The write refuses, and the move goes on to release the Project.
func TestEvictionController_EvidenceAfterTheReadKeepsTheMove(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
	env.store.request("app", "Drain", 0)
	env.reconcile(t)
	snapshots, err := env.store.ListEvictions(context.Background())
	require.NoError(t, err)
	stale := snapshots[0] // no evidence in this read

	env.store.mu.Lock()
	env.store.projects["app"].Stopped = true
	env.store.mu.Unlock()

	require.NoError(t, env.ctrl.raiseEvictionFailedAlert(context.Background(), zap.NewNop(), stale,
		EvictionFailedAgentDidNotStart, "too slow"))
	assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")), "not abandoned")
	assert.Nil(t, env.store.get("app").Failure)

	env.reconcile(t)
	assert.Equal(t, v1.EvictionStateMoving, stateOf(env.store.get("app")), "released on the evidence")
}

// A failed result's message is an error, not a backup to restore from.
func TestEvictionController_MoveMessageNamesOnlyACommittedBackup(t *testing.T) {
	for name, tc := range map[string]struct {
		result v1.ConditionStatus
		want   string
	}{
		"committed": {v1.ConditionTrue, "restored from backup b-42"},
		"failed":    {v1.ConditionFalse, "restored from its fresh backup"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newEvictionEnv(t, 1, "node-a", "node-b")
			env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
			env.store.request("app", "Drain", 0)
			env.reconcile(t)
			msg := "b-42"
			if tc.result == v1.ConditionFalse {
				msg = "upload failed: disk full"
			}
			env.store.mu.Lock()
			p := env.store.projects["app"]
			p.Stopped = true
			p.Conditions = []v1.Condition{{
				Type: v1.ConditionTypeEvictionBackup, Status: tc.result, Message: msg, LastTransitionTime: env.clock.Now(),
			}}
			env.store.mu.Unlock()
			env.reconcile(t)
			assert.Contains(t, env.store.lastMoveMessage, tc.want)
			assert.NotContains(t, env.store.lastMoveMessage, "disk full")
		})
	}
}
