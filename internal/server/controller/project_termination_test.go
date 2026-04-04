package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestProjectTerminationReconcile(t *testing.T) {
	t.Run("Terminated project is deleted from store", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{Phase: ProjectPhaseTerminated}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Len(t, ps.DeleteProjectCalls, 1)
		assert.Equal(t, "my-app", ps.DeleteProjectCalls[0].Name)
		// Verify the project was actually removed from the store.
		_, ok := ps.projects["my-app"]
		assert.False(t, ok, "project should have been deleted from store")
	})

	t.Run("project not in Terminated phase is a no-op", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{Phase: ProjectPhaseRunning}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.DeleteProjectCalls, "should not delete a non-Terminated project")
	})

	t.Run("project not found returns without error", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		// Do not add "my-app" — GetProjectPhase returns store.ErrNotFound.
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		// ProjectTerminationController explicitly handles ErrNotFound as
		// "already deleted" and returns nil.
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.DeleteProjectCalls)
	})
}
