package controller

import (
	"context"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestProjectSchedulerReconcile(t *testing.T) {
	t.Run("Pending project with one Ready node is scheduled", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending}
		ns := newFakeSchedulerNodeStore("node-1")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		assert.Equal(t, "my-app", ps.SetProjectScheduledCalls[0].Name)
		assert.Equal(t, "node-1", ps.SetProjectScheduledCalls[0].NodeRef)
	})

	t.Run("Pending project with no Ready nodes requeues", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending}
		ns := newFakeSchedulerNodeStore() // no ready nodes
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue when no Ready nodes are available")
		assert.Empty(t, ps.SetProjectScheduledCalls)
	})

	t.Run("Pending project with multiple Ready nodes is scheduled to one", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending}
		ns := newFakeSchedulerNodeStore("node-a", "node-b", "node-c")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		assert.Equal(t, "my-app", ps.SetProjectScheduledCalls[0].Name)
		// MVP algorithm picks the first node.
		assert.Equal(t, "node-a", ps.SetProjectScheduledCalls[0].NodeRef)
	})

	t.Run("project not Pending is a no-op", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhaseRunning, NodeRef: "node-1"}
		ns := newFakeSchedulerNodeStore("node-1")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.SetProjectScheduledCalls, "should not schedule a non-Pending project")
	})

	t.Run("project is placed on the node that has room", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, Size: v1.ProjectSizeLarge}
		ns := newFakeSchedulerNodeStoreOf(
			ReadyNode{Name: "small-node", Allocatable: v1.ResourceList{"cpu": "1", "memory": "1Gi"}},
			ReadyNode{Name: "big-node", Allocatable: v1.ResourceList{"cpu": "8", "memory": "16Gi"}},
		)
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		assert.Equal(t, "big-node", ps.SetProjectScheduledCalls[0].NodeRef,
			"the first node is listed first but cannot fit a Large project")
	})

	t.Run("project stays Pending and requeues when no node has room", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, Size: v1.ProjectSizeLarge}
		ns := newFakeSchedulerNodeStoreOf(
			ReadyNode{Name: "node-1", Allocatable: v1.ResourceList{"cpu": "1", "memory": "1Gi"}},
		)
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue when no node fits")
		assert.Empty(t, ps.SetProjectScheduledCalls, "must not over-commit")
		phase, nodeRef, _ := ps.GetProjectPhase(context.Background(), "my-app")
		assert.Equal(t, v1.ProjectPhasePending, phase)
		assert.Empty(t, nodeRef)
	})

	t.Run("booked capacity is counted so a node fills up", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		// Two Large projects fill a 4c / 8Gi node; the third has nowhere to go.
		for _, n := range []string{"a", "b", "c"} {
			ps.projects[n] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, Size: v1.ProjectSizeLarge}
		}
		ns := newFakeSchedulerNodeStoreOf(ReadyNode{Name: "node-1", Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}})
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		for _, n := range []string{"a", "b"} {
			res, err := ctrl.Reconcile(context.Background(), n)
			require.NoError(t, err)
			assert.False(t, res.Requeue, "project %s fits", n)
		}
		res, err := ctrl.Reconcile(context.Background(), "c")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "the node is full after two Large projects")
		assert.Len(t, ps.SetProjectScheduledCalls, 2)
	})

	t.Run("node under pressure is skipped", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending}
		ns := newFakeSchedulerNodeStoreOf(
			ReadyNode{Name: "pressured", Allocatable: roomyAllocatable,
				Conditions: []v1.Condition{{Type: v1.ConditionTypeMemoryPressure, Status: v1.ConditionTrue}}},
			ReadyNode{Name: "healthy", Allocatable: roomyAllocatable},
		)
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		_, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		assert.Equal(t, "healthy", ps.SetProjectScheduledCalls[0].NodeRef)
	})

	t.Run("node that has not reported allocatable is not used", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending}
		ns := newFakeSchedulerNodeStoreOf(ReadyNode{Name: "old-agent"})
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.True(t, res.Requeue)
		assert.Empty(t, ps.SetProjectScheduledCalls)
	})

	t.Run("a moved project is not placed back on a node it failed on", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, FailedNodes: []string{"node-a"}}
		ns := newFakeSchedulerNodeStore("node-a", "node-b")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		_, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		assert.Equal(t, "node-b", ps.SetProjectScheduledCalls[0].NodeRef,
			"node-a sorts first and has room, but the project already failed on it")
	})

	t.Run("a moved project stays Pending when every other node is excluded", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, FailedNodes: []string{"node-a"}}
		ns := newFakeSchedulerNodeStore("node-a")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.True(t, res.Requeue)
		assert.Empty(t, ps.SetProjectScheduledCalls)
	})

	t.Run("project not found returns without error", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		// Do not add "my-app" — GetProjectPhase returns store.ErrNotFound.
		ns := newFakeSchedulerNodeStore("node-1")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		// ProjectSchedulerController treats ErrNotFound as a no-op: the
		// project was deleted between Seed and Reconcile, which is a normal race.
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.SetProjectScheduledCalls)
	})
}
