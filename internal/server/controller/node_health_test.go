package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// baseTime is a fixed reference point used by all node-health tests so that
// assertions are fully deterministic and independent of wall-clock time.
var baseTime = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

func TestNodeHealthReconcile(t *testing.T) {
	t.Run("fresh heartbeat stays Ready", func(t *testing.T) {
		now := baseTime
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: now.Add(-10 * time.Second), // well within 90 s
			State:         NodeStateReady,
		}
		clk := &fakeClock{Time: now}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls, "SetNodeState should not be called for a healthy node")
	})

	t.Run("stale heartbeat transitions to NotReady", func(t *testing.T) {
		now := baseTime
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: now.Add(-2 * NodeHeartbeatTimeout), // 180 s ago
			State:         NodeStateReady,
		}
		clk := &fakeClock{Time: now}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, s.SetNodeStateCalls, 1)
		assert.Equal(t, "node-1", s.SetNodeStateCalls[0].Name)
		assert.Equal(t, NodeStateNotReady, s.SetNodeStateCalls[0].State)
		assert.Equal(t, "HeartbeatTimeout", s.SetNodeStateCalls[0].Reason)
	})

	t.Run("already NotReady with stale heartbeat is idempotent", func(t *testing.T) {
		now := baseTime
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: now.Add(-2 * NodeHeartbeatTimeout),
			State:         NodeStateNotReady,
		}
		clk := &fakeClock{Time: now}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls, "should not call SetNodeState when already NotReady")
	})

	t.Run("Draining node is skipped regardless of heartbeat", func(t *testing.T) {
		now := baseTime
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: now.Add(-2 * NodeHeartbeatTimeout),
			State:         NodeStateDraining,
		}
		clk := &fakeClock{Time: now}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, s.SetNodeStateCalls, "should not touch Draining nodes")
	})

	t.Run("recovered heartbeat transitions back to Ready", func(t *testing.T) {
		now := baseTime
		s := newFakeNodeStore()
		s.nodes["node-1"] = NodeStatusSnapshot{
			LastHeartbeat: now.Add(-10 * time.Second), // fresh heartbeat
			State:         NodeStateNotReady,
		}
		clk := &fakeClock{Time: now}
		ctrl := NewNodeHealthController(zap.NewNop(), s, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, s.SetNodeStateCalls, 1)
		assert.Equal(t, "node-1", s.SetNodeStateCalls[0].Name)
		assert.Equal(t, NodeStateReady, s.SetNodeStateCalls[0].State)
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
