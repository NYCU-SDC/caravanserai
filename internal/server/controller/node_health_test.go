package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNodeHealthReconcile(t *testing.T) {
	t.Run("fresh heartbeat stays Ready", func(t *testing.T) {
		clk := newFakeClock()
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: clk.Time.Add(-10 * time.Second), // well within 90 s
			State:         v1.NodeStateReady,
		}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls, "SetNodeState should not be called for a healthy node")
	})

	t.Run("stale heartbeat transitions to NotReady", func(t *testing.T) {
		clk := newFakeClock()
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: clk.Time.Add(-2 * NodeHeartbeatTimeout), // 180 s ago
			State:         v1.NodeStateReady,
		}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, s.SetNodeStateCalls, 1)
		assert.Equal(t, "node-1", s.SetNodeStateCalls[0].Name)
		assert.Equal(t, v1.NodeStateNotReady, s.SetNodeStateCalls[0].State)
		assert.Equal(t, "HeartbeatTimeout", s.SetNodeStateCalls[0].Reason)
	})

	t.Run("already NotReady with stale heartbeat is idempotent", func(t *testing.T) {
		clk := newFakeClock()
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: clk.Time.Add(-2 * NodeHeartbeatTimeout),
			State:         v1.NodeStateNotReady,
		}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls, "should not call SetNodeState when already NotReady")
	})

	t.Run("a Node in the legacy Draining state still goes NotReady when its heartbeat stops", func(t *testing.T) {
		// Draining is a request on the spec now (CARA-120). A node being
		// drained that dies must still be marked NotReady, or nothing would
		// ever fail its Projects over.
		clk := newFakeClock()
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: clk.Time.Add(-2 * NodeHeartbeatTimeout),
			State:         v1.NodeStateDraining,
		}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		_, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		require.Len(t, s.SetNodeStateCalls, 1)
		assert.Equal(t, v1.NodeStateNotReady, s.SetNodeStateCalls[0].State)
	})

	t.Run("recovered heartbeat transitions back to Ready", func(t *testing.T) {
		clk := newFakeClock()
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: clk.Time.Add(-10 * time.Second), // fresh heartbeat
			State:         v1.NodeStateNotReady,
		}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, s.SetNodeStateCalls, 1)
		assert.Equal(t, "node-1", s.SetNodeStateCalls[0].Name)
		assert.Equal(t, v1.NodeStateReady, s.SetNodeStateCalls[0].State)
		assert.Equal(t, "AgentReady", s.SetNodeStateCalls[0].Reason)
	})

	t.Run("node not found returns without error", func(t *testing.T) {
		s := newFakeNodeStore()
		// Do not add "node-1" — GetNodeStatus will return store.ErrNotFound.
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		// NodeHealthController treats ErrNotFound as a no-op: the node was
		// deleted between Seed and Reconcile, which is a normal race.
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls)
	})
}

// After a server outage every heartbeat is old. Within the startup grace no
// Node is marked NotReady, a NotReady Node is still marked Ready once heard
// from, and after the grace a silent Node is marked NotReady as usual.
func TestNodeHealthStartupGrace(t *testing.T) {
	clk := newFakeClock()
	s := newFakeNodeStore()
	s.nodes["silent"] = NodeStatusSnapshot{LastHeartbeat: clk.Time.Add(-time.Hour), State: v1.NodeStateReady}
	s.nodes["back"] = NodeStatusSnapshot{LastHeartbeat: clk.Time.Add(-time.Second), State: v1.NodeStateNotReady}
	ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk), WithStartupGrace(NodeHeartbeatTimeout))

	for _, n := range []string{"silent", "back"} {
		_, err := ctrl.Reconcile(context.Background(), n)
		require.NoError(t, err)
	}
	require.Len(t, s.SetNodeStateCalls, 1, "only the node heard from is changed")
	assert.Equal(t, "back", s.SetNodeStateCalls[0].Name)
	assert.Equal(t, v1.NodeStateReady, s.SetNodeStateCalls[0].State)

	clk.Time = clk.Time.Add(NodeHeartbeatTimeout - time.Second)
	_, err := ctrl.Reconcile(context.Background(), "silent")
	require.NoError(t, err)
	assert.Len(t, s.SetNodeStateCalls, 1, "still within the grace")

	clk.Time = clk.Time.Add(time.Second)
	_, err = ctrl.Reconcile(context.Background(), "silent")
	require.NoError(t, err)
	require.Len(t, s.SetNodeStateCalls, 2)
	assert.Equal(t, v1.NodeStateNotReady, s.SetNodeStateCalls[1].State)
}
