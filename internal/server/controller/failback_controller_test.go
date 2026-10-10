package controller

import (
	"context"
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
		ctrl: NewFailbackController(zap.NewNop(), st, ns, window, WithClock(clock))}
}

func tieredNode(name, tier string) ReadyNode {
	n := ReadyNode{Name: name, Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}}
	if tier != "" {
		n.Labels = map[string]string{v1.LabelNodeTier: tier}
	}
	return n
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
			env.store.running("app", "twcc1", v1.ProjectSizeSmall, true)
			env.reconcile(t)
			assert.Equal(t, tt.want, len(env.store.requests) == 1)
		})
	}
}

func TestFailback_CandidateDetection(t *testing.T) {
	t.Run("a project on backup is moved back when primary has room", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.store.running("app", "twcc1", v1.ProjectSizeMedium, true)
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
		env.store.running("born-on-backup", "twcc1", v1.ProjectSizeSmall, false)
		env.reconcile(t)
		assert.Equal(t, []string{"born-on-backup"}, env.store.requests)
	})

	t.Run("a project already on primary is left alone", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.store.running("app", "pve1", v1.ProjectSizeSmall, true)
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("an unlabeled node counts as backup", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("plain", ""))
		env.store.running("app", "plain", v1.ProjectSizeSmall, true)
		env.reconcile(t)
		assert.Equal(t, []string{"app"}, env.store.requests)
	})

	t.Run("nothing moves when primary has no room", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		env.store.running("resident-1", "pve1", v1.ProjectSizeLarge, true)
		env.store.running("resident-2", "pve1", v1.ProjectSizeLarge, true)
		env.store.running("app", "twcc1", v1.ProjectSizeSmall, true)
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("a project that only fits on a primary node it failed on is left alone", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		env.store.running("app", "twcc1", v1.ProjectSizeSmall, true)
		env.store.projects["app"].FailedNodes = []string{"pve1"}
		env.reconcile(t)
		assert.Empty(t, env.store.requests)
	})

	t.Run("a project being moved by something else is skipped", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow)
		env.store.running("a-draining", "twcc1", v1.ProjectSizeSmall, true)
		env.store.request("a-draining", "Drain", 0)
		env.store.running("b-app", "twcc1", v1.ProjectSizeSmall, true)
		env.reconcile(t)
		assert.Equal(t, []string{"b-app"}, env.store.requests)
	})

	t.Run("a too-large project does not block a smaller one", func(t *testing.T) {
		env := newFailbackEnv(t, taipeiWindow(t), inWindow, tieredNode("pve1", "primary"), tieredNode("twcc1", "backup"))
		// pve1 has 4c / 8Gi; the residents book 2.5c / 4.5Gi, leaving 1.5c.
		env.store.running("resident", "pve1", v1.ProjectSizeLarge, true)
		env.store.running("resident-2", "pve1", v1.ProjectSizeSmall, true)
		env.store.running("a-huge", "twcc1", v1.ProjectSizeLarge, true) // needs 2c: first by name, but does not fit
		env.store.running("b-small", "twcc1", v1.ProjectSizeSmall, true)
		env.reconcile(t)
		assert.Equal(t, []string{"b-small"}, env.store.requests)
	})
}

func TestFailback_OneAtATime(t *testing.T) {
	env := newFailbackEnv(t, taipeiWindow(t), inWindow)
	env.store.running("b-app", "twcc1", v1.ProjectSizeSmall, true)
	env.store.running("a-app", "twcc1", v1.ProjectSizeSmall, true)

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
	env.store.running("draining", "pve2", v1.ProjectSizeSmall, true)
	env.store.request("draining", "Drain", 0)
	env.store.running("app", "twcc1", v1.ProjectSizeSmall, true)
	env.reconcile(t)
	assert.Equal(t, []string{"app"}, env.store.requests)
}
