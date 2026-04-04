package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestProjectSchedulerReconcile(t *testing.T) {
	t.Run("Pending project with one Ready node is scheduled", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: ProjectPhasePending}
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
		ps.projects["my-app"] = schedulerProjectRecord{Phase: ProjectPhasePending}
		ns := newFakeSchedulerNodeStore() // no ready nodes
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.True(t, res.Requeue, "should requeue when no Ready nodes are available")
		assert.Empty(t, ps.SetProjectScheduledCalls)
	})

	t.Run("Pending project with multiple Ready nodes is scheduled to one", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: ProjectPhasePending}
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
		ps.projects["my-app"] = schedulerProjectRecord{Phase: ProjectPhaseRunning, NodeRef: "node-1"}
		ns := newFakeSchedulerNodeStore("node-1")
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.SetProjectScheduledCalls, "should not schedule a non-Pending project")
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
