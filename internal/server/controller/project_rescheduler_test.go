package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// reschedulerBase is a fixed reference point used by rescheduler tests so that
// assertions are fully deterministic and independent of wall-clock time.
var reschedulerBase = time.Date(2025, 6, 15, 8, 0, 0, 0, time.UTC)

func TestProjectReschedulerReconcile(t *testing.T) {
	t.Run("NotReady node with Scheduled project resets to Pending", func(t *testing.T) {
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   ProjectPhaseScheduled,
			NodeRef: "node-1",
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectPendingCalls, 1)
		assert.Equal(t, "app-1", ps.SetProjectPendingCalls[0].Name)
	})

	t.Run("NotReady node with Running project resets to Pending", func(t *testing.T) {
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   ProjectPhaseRunning,
			NodeRef: "node-1",
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectPendingCalls, 1)
		assert.Equal(t, "app-1", ps.SetProjectPendingCalls[0].Name)
	})

	t.Run("NotReady node with Terminating project without TerminatingAt sets condition", func(t *testing.T) {
		// Scenario: a project was in Terminating phase (node was tearing down
		// containers) when the node went NotReady. This is the first time the
		// rescheduler sees this situation, so no TerminatingAt condition exists.
		//
		// Expected behaviour (handleTerminating, !found branch):
		//   1. Record a TerminatingAt condition with the current timestamp —
		//      this starts the 10-minute force-termination clock.
		//   2. Requeue so the manager checks again after the timeout elapses.
		//   3. Do NOT force-terminate yet — the node might recover in time.
		now := reschedulerBase
		clk := &fakeClock{Time: now}
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   ProjectPhaseTerminating,
			NodeRef: "node-1",
			// No conditions — first observation.
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue to check timeout later")
		require.Len(t, ps.SetTerminatingAtCalls, 1)
		assert.Equal(t, "app-1", ps.SetTerminatingAtCalls[0].Name)
		assert.Equal(t, now.UTC(), ps.SetTerminatingAtCalls[0].At, "should record the fake clock's time")
		assert.Empty(t, ps.ForceTerminatedCalls, "should not force-terminate on first observation")
	})

	t.Run("NotReady node with Terminating project past timeout forces Terminated", func(t *testing.T) {
		// Scenario: the TerminatingAt condition was recorded 11 minutes ago
		// (exceeding the 10-minute terminatingTimeout). The node never
		// recovered, so the graceful teardown is assumed to have failed.
		//
		// Expected behaviour (handleTerminating, elapsed >= terminatingTimeout):
		//   1. Call ForceTerminated — transitions the project to Terminated
		//      phase so ProjectTerminationController can delete the DB record.
		//   2. Do NOT requeue — this project is done.
		//   3. Do NOT re-write TerminatingAt — the condition already exists.
		now := reschedulerBase
		terminatingAt := now.Add(-11 * time.Minute) // past 10-min timeout
		clk := &fakeClock{Time: now}
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   ProjectPhaseTerminating,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               condTypeTerminatingAt,
					LastTransitionTime: terminatingAt,
				},
			},
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.ForceTerminatedCalls, 1)
		assert.Equal(t, "app-1", ps.ForceTerminatedCalls[0].Name)
		assert.Empty(t, ps.SetTerminatingAtCalls, "should not re-set TerminatingAt")
	})

	t.Run("NotReady node with Terminating project within timeout requeues", func(t *testing.T) {
		// Scenario: the TerminatingAt condition was recorded 5 minutes ago.
		// The 10-minute timeout has NOT elapsed yet — the node might still
		// come back and finish the graceful teardown.
		//
		// Expected behaviour (handleTerminating, elapsed < terminatingTimeout):
		//   1. Requeue — the manager will re-run Reconcile later to check
		//      whether the timeout has now expired.
		//   2. Do NOT force-terminate — still within the grace period.
		//   3. Do NOT re-write TerminatingAt — the condition already exists
		//      and its timestamp must not be reset (that would restart the
		//      clock and potentially let a project stay in Terminating forever).
		now := reschedulerBase
		terminatingAt := now.Add(-5 * time.Minute) // within 10-min timeout
		clk := &fakeClock{Time: now}
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   ProjectPhaseTerminating,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               condTypeTerminatingAt,
					LastTransitionTime: terminatingAt,
				},
			},
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue to check again later")
		assert.Empty(t, ps.ForceTerminatedCalls, "should not force-terminate before timeout")
		assert.Empty(t, ps.SetTerminatingAtCalls, "condition already exists, no re-write needed")
	})

	t.Run("Ready node is a no-op", func(t *testing.T) {
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: NodeStateReady}
		ps := newFakeReschedulerProjectStore()
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.SetProjectPendingCalls)
		assert.Empty(t, ps.SetTerminatingAtCalls)
		assert.Empty(t, ps.ForceTerminatedCalls)
	})

	t.Run("node not found returns without error", func(t *testing.T) {
		ns := newFakeReschedulerNodeStore()
		// Do not add "node-1" — GetNodeStatus returns store.ErrNotFound.
		ps := newFakeReschedulerProjectStore()
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		// ProjectReschedulerController explicitly handles ErrNotFound as
		// "node deleted" and returns nil.
		require.NoError(t, err)
		assert.False(t, res.Requeue)
	})
}
