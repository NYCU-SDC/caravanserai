package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/dailywindow"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// taipei is the window the tests use: 02:00-05:00 Taiwan, 18:00-21:00 UTC.
func taipeiWindow(t *testing.T) *dailywindow.Window {
	t.Helper()
	w, err := dailywindow.Parse("02:00-05:00", "Asia/Taipei")
	require.NoError(t, err)
	return &w
}

var (
	inWindow  = time.Date(2026, 10, 9, 18, 30, 0, 0, time.UTC) // 02:30 Taiwan
	outWindow = time.Date(2026, 10, 10, 2, 30, 0, 0, time.UTC) // 10:30 Taiwan
)

// failbackEnv is a fail-back controller over a fake store, with pve1 and pve2
// on the primary tier and twcc1 on backup, each 4c / 8Gi.
type failbackEnv struct {
	clock *fakeClock
	store *fakeEvictionStore
	nodes *fakeSchedulerNodeStore
	ctrl  *FailbackController
}

func newFailbackEnv(t *testing.T, window *dailywindow.Window, at time.Time, nodes ...ReadyNode) *failbackEnv {
	t.Helper()
	if nodes == nil {
		nodes = []ReadyNode{tieredNode("pve1", "primary"), tieredNode("pve2", "primary"), tieredNode("twcc1", "backup")}
	}
	clock := &fakeClock{Time: at}
	st := newFakeEvictionStore(clock)
	ns := newFakeSchedulerNodeStoreOf(nodes...)
	return &failbackEnv{clock: clock, store: st, nodes: ns,
		ctrl: NewFailbackController(zap.NewNop(), st, ns, window, DefaultFailbackPolicy(), WithClock(clock))}
}

// longAgo is well past every default wait, so a node Ready since then is
// stable and a project running since then has cooled down.
var longAgo = inWindow.Add(-72 * time.Hour)

// tieredNode is a 4c / 8Gi node of tier, Ready since longAgo.
func tieredNode(name, tier string) ReadyNode {
	return readySince(name, tier, longAgo)
}

func readySince(name, tier string, since time.Time) ReadyNode {
	n := ReadyNode{
		Name:        name,
		Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"},
		Conditions:  []v1.Condition{{Type: v1.ConditionTypeReady, Status: v1.ConditionTrue, LastTransitionTime: since}},
	}
	if tier != "" {
		n.Labels = map[string]string{v1.LabelNodeTier: tier}
	}
	return n
}

// running adds a project running on node since longAgo.
func (e *failbackEnv) running(name, node string, size v1.ProjectSize) {
	e.runningSince(name, node, size, longAgo)
}

func (e *failbackEnv) runningSince(name, node string, size v1.ProjectSize, since time.Time) {
	e.store.running(name, node, size, true)
	e.store.projects[name].Conditions = []v1.Condition{{
		Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: phaseReasonRunning, LastTransitionTime: since,
	}}
}

func (e *failbackEnv) reconcile(t *testing.T) {
	t.Helper()
	_, err := e.ctrl.Reconcile(context.Background(), failbackQueueKey)
	require.NoError(t, err)
}

func TestFailback_WindowGating(t *testing.T) {
	tests := []struct {
		name   string
		window *dailywindow.Window
		at     time.Time
		want   bool
	}{
		{name: "inside the window", window: taipeiWindow(t), at: inWindow, want: true},
		{name: "outside the window", window: taipeiWindow(t), at: outWindow},
		{name: "no window configured", window: nil, at: inWindow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFailbackEnv(t, tt.window, tt.at)
			env.running("app", "twcc1", v1.ProjectSizeSmall)
			env.reconcile(t)
			assert.Equal(t, tt.want, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_CandidateDetection(t *testing.T) {
	t.Run("a project on backup is moved back when primary has room", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.running("app", "twcc1", v1.ProjectSizeMedium)
		env.reconcile(t)

		require.Equal(t, []string{"app"}, env.store.requests)
		e := env.store.get("app").Eviction
		assert.Equal(t, FailBackReason, e.Reason)
		assert.Equal(t, "twcc1", e.FromNode)
		assert.Equal(t, v1.NodeTierPrimary, e.TargetTier, "a fail-back must land on primary")
	})

	t.Run("detection needs no record of where it ran before", func(t *testing.T) {
		// The project has only ever run on twcc1; nothing says it came from
		// primary. Being on backup while primary has room is enough.
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.running("born-on-backup", "twcc1", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Equal(t, []string{"born-on-backup"}, env.store.requests)
	})

	t.Run("a project already on primary is left alone", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.running("app", "pve1", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("an unlabeled node counts as backup", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("plain", ""))
		env.running("app", "plain", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Equal(t, []string{"app"}, env.store.requests)
	})

	t.Run("nothing moves when primary has no room", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		env.running("resident-1", "pve1", v1.ProjectSizeLarge)
		env.running("resident-2", "pve1", v1.ProjectSizeLarge)
		env.running("app", "twcc1", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("a project that only fits on a primary node it failed on is left alone", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		env.running("app", "twcc1", v1.ProjectSizeSmall)
		env.store.projects["app"].FailedNodes = []string{"pve1"}
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("a project being moved by something else is skipped", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.running("a-draining", "twcc1", v1.ProjectSizeSmall)
		env.store.request("a-draining", "Drain", 0)
		env.running("b-app", "twcc1", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Equal(t, []string{"b-app"}, env.store.requests)
	})

	t.Run("a too-large project does not block a smaller one", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		// pve1 has 4c / 8Gi; the residents book 2.5c / 4.5Gi, leaving 1.5c.
		env.running("resident", "pve1", v1.ProjectSizeLarge)
		env.running("resident-2", "pve1", v1.ProjectSizeSmall)
		env.running("a-huge", "twcc1", v1.ProjectSizeLarge) // needs 2c: first by name, but does not fit
		env.running("b-small", "twcc1", v1.ProjectSizeSmall)
		env.reconcile(t)
		assert.Equal(t, []string{"b-small"}, env.store.requests)
	})
}

func TestFailback_OneAtATime(t *testing.T) {
	env := newFailbackEnv(t, taipeiWindow(t), inWindow)
	env.running("b-app", "twcc1", v1.ProjectSizeSmall)
	env.running("a-app", "twcc1", v1.ProjectSizeSmall)

	env.reconcile(t)
	assert.Equal(t, []string{"a-app"}, env.store.requests, "name order, one request")

	env.reconcile(t)
	assert.Equal(t, []string{"a-app"}, env.store.requests, "the next waits while a fail-back move is in progress")

	// The first move lands on primary and ends.
	env.store.mu.Lock()
	env.store.projects["a-app"].Eviction = nil
	env.store.projects["a-app"].NodeRef = "pve1"
	env.store.mu.Unlock()
	env.reconcile(t)
	assert.Equal(t, []string{"a-app", "b-app"}, env.store.requests)
}

func TestFailback_OtherMovesDoNotHoldItBack(t *testing.T) {
	// Only a fail-back move in progress makes fail-back wait; a drain elsewhere
	// is the eviction controller's to queue.
	env := newFailbackEnv(t, taipeiWindow(t), inWindow)
	env.running("draining", "pve2", v1.ProjectSizeSmall)
	env.store.request("draining", "Drain", 0)
	env.running("app", "twcc1", v1.ProjectSizeSmall)
	env.reconcile(t)
	assert.Equal(t, []string{"app"}, env.store.requests)
}

func TestFailbackPolicy_StableWindowFor(t *testing.T) {
	p := DefaultFailbackPolicy()
	tests := []struct {
		flaps int
		want  time.Duration
	}{
		{0, 30 * time.Minute},
		{1, 30 * time.Minute},
		{2, time.Hour},
		{3, 2 * time.Hour},
		{4, 4 * time.Hour},
		{5, 8 * time.Hour},
		{6, 8 * time.Hour},
		{100, 8 * time.Hour},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, p.stableWindowFor(tt.flaps), "flaps=%d", tt.flaps)
	}

	assert.Equal(t, FailbackPolicy{
		StableWindow: 30 * time.Minute, FlapWindow: 24 * time.Hour, BackoffMultiplier: 2,
		MaxStableWindow: 8 * time.Hour, Cooldown: 6 * time.Hour, MinFreePercent: 20,
	}, p, "interim defaults pending sign-off")
}

// lastRequest returns the eviction fail-back asked for, or nil.
func (e *failbackEnv) lastRequest(name string) *v1.ProjectEviction {
	return e.store.get(name).Eviction
}

func TestFailback_StableWindow(t *testing.T) {
	tests := []struct {
		name     string
		primary  ReadyNode
		wantMove bool
	}{
		{name: "ready 29 minutes is not enough", primary: readySince("pve1", "primary", inWindow.Add(-29*time.Minute))},
		{name: "ready 30 minutes is", primary: readySince("pve1", "primary", inWindow.Add(-30*time.Minute)), wantMove: true},
		{
			name: "ready time unknown is not trusted",
			primary: func() ReadyNode {
				n := tieredNode("pve1", "primary")
				n.Conditions = nil
				return n
			}(),
		},
		{
			name: "a Ready condition still saying False is not trusted",
			primary: func() ReadyNode {
				n := tieredNode("pve1", "primary")
				n.Conditions[0].Status = v1.ConditionFalse
				return n
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFailbackEnv(t, taipeiWindow(t), inWindow, tt.primary, tieredNode("twcc1", "backup"))
			env.running("app", "twcc1", v1.ProjectSizeSmall)
			env.reconcile(t)
			assert.Equal(t, tt.wantMove, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_FlapBackoff(t *testing.T) {
	flapped := func(readyFor time.Duration, flapsAgo ...time.Duration) ReadyNode {
		n := readySince("pve1", "primary", inWindow.Add(-readyFor))
		for _, ago := range flapsAgo {
			n.RecentNotReady = append(n.RecentNotReady, inWindow.Add(-ago))
		}
		return n
	}
	tests := []struct {
		name     string
		primary  ReadyNode
		wantMove bool
	}{
		{name: "two flaps need an hour: 59 minutes is not enough", primary: flapped(59*time.Minute, 3*time.Hour, 61*time.Minute)},
		{name: "two flaps need an hour", primary: flapped(time.Hour, 3*time.Hour, 61*time.Minute), wantMove: true},
		{name: "flaps older than the flap window do not count", primary: flapped(30*time.Minute, 30*time.Hour, 26*time.Hour), wantMove: true},
		{
			name:    "five flaps hit the eight hour cap",
			primary: flapped(7*time.Hour+59*time.Minute, 20*time.Hour, 16*time.Hour, 12*time.Hour, 10*time.Hour, 8*time.Hour),
		},
		{
			name:     "and eight hours is enough however many",
			primary:  flapped(8*time.Hour, 20*time.Hour, 16*time.Hour, 14*time.Hour, 12*time.Hour, 10*time.Hour, 9*time.Hour),
			wantMove: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFailbackEnv(t, taipeiWindow(t), inWindow, tt.primary, tieredNode("twcc1", "backup"))
			env.running("app", "twcc1", v1.ProjectSizeSmall)
			env.reconcile(t)
			assert.Equal(t, tt.wantMove, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_UnstablePrimaryIsAvoided(t *testing.T) {
	// pve2 has just recovered and is empty, so the scheduler, which prefers
	// the emptiest node, would choose it. The request must rule it out.
	env := newFailbackEnv(t, taipeiWindow(t), inWindow,
		tieredNode("pve1", "primary"), readySince("pve2", "primary", inWindow.Add(-5*time.Minute)), tieredNode("twcc1", "backup"))
	env.running("resident", "pve1", v1.ProjectSizeMedium)
	env.running("app", "twcc1", v1.ProjectSizeSmall)
	env.reconcile(t)

	e := env.lastRequest("app")
	require.NotNil(t, e)
	assert.Equal(t, []string{"pve2"}, e.AvoidNodes)
}

func TestFailback_Cooldown(t *testing.T) {
	tests := []struct {
		name     string
		since    time.Duration
		reason   string
		wantMove bool
	}{
		{name: "running 5h59m on backup is too recent", since: 5*time.Hour + 59*time.Minute, reason: phaseReasonRunning},
		{name: "running 6h has cooled down", since: 6 * time.Hour, reason: phaseReasonRunning, wantMove: true},
		{name: "running time unknown is left alone", since: 24 * time.Hour, reason: "Evicted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFailbackEnv(t, taipeiWindow(t), inWindow)
			env.runningSince("app", "twcc1", v1.ProjectSizeSmall, inWindow.Add(-tt.since))
			env.store.projects["app"].Conditions[0].Reason = tt.reason
			env.reconcile(t)
			assert.Equal(t, tt.wantMove, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_Hysteresis(t *testing.T) {
	node := func(name, cpu, mem string) ReadyNode {
		n := tieredNode(name, "primary")
		n.Allocatable = v1.ResourceList{"cpu": cpu, "memory": mem}
		return n
	}
	tests := []struct {
		name      string
		primary   ReadyNode
		residents []v1.ProjectSize
		size      v1.ProjectSize
		wantMove  bool
	}{
		{
			// 2c / 4Gi booked of 4c / 8Gi; a Medium leaves 1c / 2Gi = 25%.
			name: "comfortable room", primary: node("pve1", "4", "8Gi"),
			residents: []v1.ProjectSize{v1.ProjectSizeLarge}, size: v1.ProjectSizeMedium, wantMove: true,
		},
		{
			// 2.5c booked of 4c; a Medium fits, leaving 0.5c = 12.5%.
			name: "fits but only just", primary: node("pve1", "4", "8Gi"),
			residents: []v1.ProjectSize{v1.ProjectSizeLarge, v1.ProjectSizeSmall}, size: v1.ProjectSizeMedium,
		},
		{
			// 2c / 4Gi booked of 5c / 10Gi; a Large leaves 1c / 2Gi = exactly 20%.
			name: "exactly the threshold", primary: node("pve1", "5", "10Gi"),
			residents: []v1.ProjectSize{v1.ProjectSizeLarge}, size: v1.ProjectSizeLarge, wantMove: true,
		},
		{
			// CPU leaves 50%, memory 2Gi + 2Gi booked of 4.5Gi leaves under 12%.
			name: "memory short though CPU is ample", primary: node("pve1", "8", "4608Mi"),
			residents: []v1.ProjectSize{v1.ProjectSizeMedium}, size: v1.ProjectSizeMedium,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newFailbackEnv(t, taipeiWindow(t), inWindow, tt.primary, tieredNode("twcc1", "backup"))
			for i, sz := range tt.residents {
				env.running(fmt.Sprintf("resident-%d", i), "pve1", sz)
			}
			env.running("app", "twcc1", tt.size)
			env.reconcile(t)
			assert.Equal(t, tt.wantMove, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_TightPrimaryIsAvoided(t *testing.T) {
	// pve1 can take the project but would be left nearly full; pve2 has
	// plenty of room. The move is asked for, ruling pve1 out.
	env := newFailbackEnv(t, taipeiWindow(t), inWindow,
		tieredNode("pve1", "primary"), tieredNode("pve2", "primary"), tieredNode("twcc1", "backup"))
	env.running("resident-1", "pve1", v1.ProjectSizeLarge)
	env.running("resident-2", "pve1", v1.ProjectSizeSmall)
	env.running("app", "twcc1", v1.ProjectSizeMedium)
	env.reconcile(t)

	e := env.lastRequest("app")
	require.NotNil(t, e)
	assert.Equal(t, []string{"pve1"}, e.AvoidNodes)
}
