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

func TestProjectReschedulerReconcile(t *testing.T) {
	t.Run("NotReady node with Scheduled project resets to Pending", func(t *testing.T) {
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseScheduled,
			NodeRef: "node-1",
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectPendingCalls, 1)
		assert.Equal(t, "app-1", ps.SetProjectPendingCalls[0].Name)
	})

	t.Run("NotReady node with Running project records NotReadyAt on first observation, requeues", func(t *testing.T) {
		// Scenario: a Running project on a NotReady node — first time the
		// rescheduler sees this situation, so no NotReadyAt condition exists.
		//
		// Expected behaviour (handleRunning, !found branch):
		//   1. Record a NotReadyAt condition with the current timestamp —
		//      this starts the grace period clock.
		//   2. Requeue so the manager checks again after the grace period.
		//   3. Do NOT reset to Pending yet — the node might recover.
		clk := newFakeClock()
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseRunning,
			NodeRef: "node-1",
			// No conditions — first observation.
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue to check grace period later")
		require.Len(t, ps.SetNotReadyAtCalls, 1)
		assert.Equal(t, "app-1", ps.SetNotReadyAtCalls[0].Name)
		assert.Equal(t, clk.Time.UTC(), ps.SetNotReadyAtCalls[0].At, "should record the fake clock's time")
		assert.Empty(t, ps.SetProjectPendingCalls, "should not reset to Pending on first observation")
	})

	t.Run("NotReady node with Running project within grace period requeues", func(t *testing.T) {
		// Scenario: the NotReadyAt condition was recorded halfway through the
		// grace period. The grace period has NOT elapsed yet — the node might
		// still come back.
		//
		// Expected behaviour (handleRunning, elapsed < runningGracePeriod):
		//   1. Requeue — the manager will re-run Reconcile later to check
		//      whether the grace period has now expired.
		//   2. Do NOT reset to Pending — still within the grace period.
		//   3. Do NOT re-write NotReadyAt — the condition already exists
		//      and its timestamp must not be reset.
		clk := newFakeClock()
		notReadyAt := clk.Time.Add(-runningGracePeriod / 2) // within grace period
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseRunning,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               v1.ConditionTypeNotReadyAt,
					LastTransitionTime: notReadyAt,
				},
			},
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue to check again later")
		assert.Empty(t, ps.SetProjectPendingCalls, "should not reset to Pending before grace period expires")
		assert.Empty(t, ps.SetNotReadyAtCalls, "condition already exists, no re-write needed")
	})

	t.Run("NotReady node with Running project past grace period resets to Pending", func(t *testing.T) {
		// Scenario: the NotReadyAt condition was recorded past the grace period.
		// The node never recovered, so the project should be reset to Pending.
		//
		// Expected behaviour (handleRunning, elapsed >= runningGracePeriod):
		//   1. Call SetProjectPending — resets the project to Pending so the
		//      scheduler can place it on a healthy node.
		//   2. Do NOT requeue — this project is done.
		//   3. Do NOT re-write NotReadyAt — the condition already exists.
		clk := newFakeClock()
		notReadyAt := clk.Time.Add(-runningGracePeriod - time.Minute) // past grace period
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseRunning,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               v1.ConditionTypeNotReadyAt,
					LastTransitionTime: notReadyAt,
				},
			},
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectPendingCalls, 1)
		assert.Equal(t, "app-1", ps.SetProjectPendingCalls[0].Name)
		assert.Empty(t, ps.SetNotReadyAtCalls, "should not re-set NotReadyAt")
	})

	t.Run("NotReady node with Terminating project without TerminatingAt sets condition", func(t *testing.T) {
		// Scenario: a project was in Terminating phase (node was tearing down
		// containers) when the node went NotReady. This is the first time the
		// rescheduler sees this situation, so no TerminatingAt condition exists.
		//
		// Expected behaviour (handleTerminating, !found branch):
		//   1. Record a TerminatingAt condition with the current timestamp —
		//      this starts the force-termination clock.
		//   2. Requeue so the manager checks again after the timeout elapses.
		//   3. Do NOT force-terminate yet — the node might recover in time.
		clk := newFakeClock()
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseTerminating,
			NodeRef: "node-1",
			// No conditions — first observation.
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue to check timeout later")
		require.Len(t, ps.SetTerminatingAtCalls, 1)
		assert.Equal(t, "app-1", ps.SetTerminatingAtCalls[0].Name)
		assert.Equal(t, clk.Time.UTC(), ps.SetTerminatingAtCalls[0].At, "should record the fake clock's time")
		assert.Empty(t, ps.ForceTerminatedCalls, "should not force-terminate on first observation")
	})

	t.Run("NotReady node with Terminating project past timeout forces Terminated", func(t *testing.T) {
		// Scenario: the TerminatingAt condition was recorded past the timeout.
		// The node never recovered, so the graceful teardown is assumed to have failed.
		//
		// Expected behaviour (handleTerminating, elapsed >= terminatingTimeout):
		//   1. Call ForceTerminated — transitions the project to Terminated
		//      phase so ProjectTerminationController can delete the DB record.
		//   2. Do NOT requeue — this project is done.
		//   3. Do NOT re-write TerminatingAt — the condition already exists.
		clk := newFakeClock()
		terminatingAt := clk.Time.Add(-terminatingTimeout - time.Minute) // past timeout
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseTerminating,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               v1.ConditionTypeTerminatingAt,
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
		// Scenario: the TerminatingAt condition was recorded halfway through the
		// timeout. The timeout has NOT elapsed yet — the node might still
		// come back and finish the graceful teardown.
		//
		// Expected behaviour (handleTerminating, elapsed < terminatingTimeout):
		//   1. Requeue — the manager will re-run Reconcile later to check
		//      whether the timeout has now expired.
		//   2. Do NOT force-terminate — still within the grace period.
		//   3. Do NOT re-write TerminatingAt — the condition already exists
		//      and its timestamp must not be reset (that would restart the
		//      clock and potentially let a project stay in Terminating forever).
		clk := newFakeClock()
		terminatingAt := clk.Time.Add(-terminatingTimeout / 2) // within timeout
		ns := newFakeReschedulerNodeStore()
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateNotReady}
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name:    "app-1",
			Phase:   v1.ProjectPhaseTerminating,
			NodeRef: "node-1",
			Conditions: []ConditionSnapshot{
				{
					Type:               v1.ConditionTypeTerminatingAt,
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
		ns.nodes["node-1"] = NodeStatusSnapshot{State: v1.NodeStateReady}
		ps := newFakeReschedulerProjectStore()
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "node-1")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.SetProjectPendingCalls)
		assert.Empty(t, ps.SetTerminatingAtCalls)
		assert.Empty(t, ps.SetNotReadyAtCalls)
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

// The grace period must protect a Project every time its node fails, not only
// the first time.
//
// NotReadyAt is a clock, and nothing in the system removed it once the
// incident was over. On the second failure the elapsed time was therefore
// measured from the first one — hours old by then, always past the grace
// period — so the Project was reset to Pending the instant the node was
// marked NotReady. The wait that exists to absorb a brief blip worked once per
// Project and then silently stopped.
func TestProjectReschedulerGracePeriodIsPerIncident(t *testing.T) {
	// notReadyAfter returns the node state the health controller would have
	// written: NotReady, with the last heartbeat one timeout ago.
	notReadyAfter := func(lastBeat time.Time) NodeStatusSnapshot {
		return NodeStatusSnapshot{State: v1.NodeStateNotReady, LastHeartbeat: lastBeat}
	}

	t.Run("a second failure serves the full grace period", func(t *testing.T) {
		clk := newFakeClock()
		ns := newFakeReschedulerNodeStore()
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name: "app-1", Phase: v1.ProjectPhaseRunning, NodeRef: "node-1",
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))
		ctx := context.Background()

		// --- Incident 1. The node stopped beating 90s ago. ---
		firstBeat := clk.Time.Add(-NodeHeartbeatTimeout)
		ns.nodes["node-1"] = notReadyAfter(firstBeat)
		res, err := ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		require.Len(t, ps.SetNotReadyAtCalls, 1, "the first failure starts a clock")
		assert.True(t, res.Requeue)
		assert.Empty(t, ps.SetProjectPendingCalls, "the grace period has barely begun")

		// --- The node recovers inside the grace period and beats again. ---
		clk.Time = clk.Time.Add(time.Minute)
		recoveredBeat := clk.Time

		// --- Hours of health, then incident 2. ---
		clk.Time = clk.Time.Add(4 * time.Hour)
		ns.nodes["node-1"] = notReadyAfter(recoveredBeat)

		// The stale clock is still on the Project: nothing removed it, which
		// is exactly the situation this rule has to survive.
		require.Len(t, ps.projects["app-1"].Conditions, 1)
		require.Equal(t, v1.ConditionTypeNotReadyAt, ps.projects["app-1"].Conditions[0].Type)

		res, err = ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		require.Len(t, ps.SetNotReadyAtCalls, 2, "a clock predating the last heartbeat is restarted")
		assert.Equal(t, clk.Time.UTC(), ps.SetNotReadyAtCalls[1].At)
		assert.True(t, res.Requeue)
		assert.Empty(t, ps.SetProjectPendingCalls,
			"the second failure must wait, not inherit the first failure's elapsed time")

		// --- One second short of the new grace period: still holding. ---
		clk.Time = clk.Time.Add(runningGracePeriod - time.Second)
		_, err = ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		assert.Empty(t, ps.SetProjectPendingCalls)
		assert.Len(t, ps.SetNotReadyAtCalls, 2, "the clock is not restarted within one incident")

		// --- Past it: now, and only now, reschedule. ---
		clk.Time = clk.Time.Add(2 * time.Second)
		_, err = ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		require.Len(t, ps.SetProjectPendingCalls, 1)
		assert.Equal(t, "app-1", ps.SetProjectPendingCalls[0].Name)
	})

	t.Run("a clock from the current failure is trusted across reconciles", func(t *testing.T) {
		// The node stays down, so its last heartbeat does not move and the
		// clock keeps accumulating. Restarting it here would mean the grace
		// period never expires and the Project is never moved off a dead node.
		clk := newFakeClock()
		ns := newFakeReschedulerNodeStore()
		ps := newFakeReschedulerProjectStore()
		ps.projects["app-1"] = &ProjectSnapshot{
			Name: "app-1", Phase: v1.ProjectPhaseRunning, NodeRef: "node-1",
		}
		ctrl := NewProjectReschedulerController(zap.NewNop(), ps, ns, nil, WithClock(clk))
		ctx := context.Background()

		lastBeat := clk.Time.Add(-NodeHeartbeatTimeout)
		ns.nodes["node-1"] = notReadyAfter(lastBeat)

		_, err := ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		require.Len(t, ps.SetNotReadyAtCalls, 1)

		for elapsed := 30 * time.Second; elapsed < runningGracePeriod; elapsed += 30 * time.Second {
			clk.Time = clk.Time.Add(30 * time.Second)
			_, err = ctrl.Reconcile(ctx, "node-1")
			require.NoError(t, err)
			require.Len(t, ps.SetNotReadyAtCalls, 1, "the clock must not restart mid-incident")
			require.Empty(t, ps.SetProjectPendingCalls)
		}

		clk.Time = clk.Time.Add(runningGracePeriod)
		_, err = ctrl.Reconcile(ctx, "node-1")
		require.NoError(t, err)
		assert.Len(t, ps.SetProjectPendingCalls, 1, "the grace period still expires")
	})
}
