package controller

import (
	"context"
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
	Phase       v1.ProjectPhase
	NodeRef     string
	Size        v1.ProjectSize
	Managed     bool
	Eviction    *v1.ProjectEviction
	Conditions  []v1.Condition
	Failure     *EvictionFailure
	FailedNodes []string
}

// fakeEvictionStore mirrors the adapter's guards closely enough to drive the
// controller through whole moves, and serves the capacity ledger from the
// same Projects.
type fakeEvictionStore struct {
	mu       sync.Mutex
	clock    *fakeClock
	projects map[string]*fakeEvictionProject

	backupsAsked []string
	requests     []string
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
	p.Conditions = append(p.Conditions, v1.Condition{
		Type: v1.ConditionTypeEvictionBackup, Status: status, Message: message, LastTransitionTime: f.clock.Now(),
	})
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
			conds := make([]ConditionSnapshot, len(p.Conditions))
			for i, c := range p.Conditions {
				conds[i] = ConditionSnapshot{Type: c.Type, Reason: c.Reason, LastTransitionTime: c.LastTransitionTime}
			}
			out = append(out, &ProjectSnapshot{
				Name: n, Phase: p.Phase, NodeRef: p.NodeRef, Size: p.Size,
				FailedNodes: p.FailedNodes, Evicting: p.Eviction != nil, Conditions: conds,
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

func (f *fakeEvictionStore) MoveEvictedProject(_ context.Context, name, fromNode, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	if p.Eviction == nil || p.Phase != v1.ProjectPhaseRunning || p.NodeRef != fromNode {
		return nil
	}
	p.Eviction.State, p.Eviction.StateSince = v1.EvictionStateMoving, f.clock.Now()
	p.Phase, p.NodeRef = v1.ProjectPhasePending, ""
	return nil
}

// RequestEviction mirrors the adapter's guards.
func (f *fakeEvictionStore) RequestEviction(_ context.Context, name, fromNode, reason string, tier v1.NodeTier, avoid []string) error {
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
	p.Eviction = &v1.ProjectEviction{Reason: reason, FromNode: fromNode, State: v1.EvictionStateQueued, TargetTier: tier, AvoidNodes: avoid, RequestedAt: now, StateSince: now}
	f.requests = append(f.requests, name)
	return nil
}

func (f *fakeEvictionStore) EndEviction(_ context.Context, name string, failure *EvictionFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	p.Eviction = nil
	p.Failure = failure
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

func TestEvictionController_ProjectWithoutDataSkipsTheBackup(t *testing.T) {
	env := newEvictionEnv(t, 1, "node-a", "node-b")
	env.store.running("app", "node-a", v1.ProjectSizeSmall, false)
	env.store.request("app", "FailBack", 0)

	env.reconcile(t)
	p := env.store.get("app")
	assert.Equal(t, v1.EvictionStateMoving, stateOf(p))
	assert.Equal(t, v1.ProjectPhasePending, p.Phase)
	assert.Empty(t, env.store.backupsAsked)
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
		env.store.agentReports("app", v1.ConditionTrue, "old")
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

func TestEvictionController_AvoidNodes(t *testing.T) {
	t.Run("the precheck does not count nodes the move must avoid", func(t *testing.T) {
		env := newTieredEvictionEnv(t, map[string]string{"node-a": "backup", "node-b": "primary"})
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "FailBack", 0)
		env.store.projects["app"].Eviction.TargetTier = v1.NodeTierPrimary
		env.store.projects["app"].Eviction.AvoidNodes = []string{"node-b"}

		env.reconcile(t)
		p := env.store.get("app")
		require.NotNil(t, p.Failure)
		assert.Equal(t, EvictionFailedNoCapacity, p.Failure.Reason)
		assert.Empty(t, env.store.backupsAsked)
	})

	t.Run("another node in the tier is still a candidate", func(t *testing.T) {
		env := newTieredEvictionEnv(t, map[string]string{"node-a": "backup", "node-b": "primary", "node-c": "primary"})
		env.store.running("app", "node-a", v1.ProjectSizeSmall, true)
		env.store.request("app", "FailBack", 0)
		env.store.projects["app"].Eviction.TargetTier = v1.NodeTierPrimary
		env.store.projects["app"].Eviction.AvoidNodes = []string{"node-b"}

		env.reconcile(t)
		assert.Equal(t, v1.EvictionStateBackingUp, stateOf(env.store.get("app")))
	})
}
