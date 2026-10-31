package controller

import (
	"context"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestProjectTerminationReconcile(t *testing.T) {
	t.Run("Terminated project is finalized for the lifetime it was read as", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminated}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Equal(t, []finalizeCall{{Name: "my-app", UID: "uid-1"}}, ps.FinalizeCalls)
		_, ok := ps.projects["my-app"]
		assert.False(t, ok, "project should have been deleted from store")
		assert.Empty(t, ps.SetFinalizationBlockedCalls)
	})

	t.Run("project not in Terminated phase is a no-op", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminating}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.FinalizeCalls, "should not finalize a non-Terminated project")
	})

	t.Run("project not found returns without error", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.FinalizeCalls)
	})

	t.Run("a record the predicate refuses is kept and says what it waits for", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{
			UID: "uid-1", Phase: v1.ProjectPhaseTerminated,
			BlockedBy: "waiting for ResourcesRemoved from node node-b generation 1",
		}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err, "a blocked finalization is not an error to retry")
		assert.False(t, res.Requeue)
		_, ok := ps.projects["my-app"]
		assert.True(t, ok, "a blocked record must stay")
		require.Equal(t, []setFinalizationBlockedCall{{
			Name: "my-app", UID: "uid-1",
			Reason: "waiting for ResourcesRemoved from node node-b generation 1",
		}}, ps.SetFinalizationBlockedCalls)
	})

	t.Run("a same-name Project created after the read is not finalized", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminated}
		ctrl := NewProjectTerminationController(zap.NewNop(), &recreateAfterRead{fakeTerminationProjectStore: ps}, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		r, ok := ps.projects["my-app"]
		require.True(t, ok, "the new lifetime must not be deleted")
		assert.Equal(t, "uid-2", r.UID)
		assert.Empty(t, ps.SetFinalizationBlockedCalls)
	})
}

// recreateAfterRead replaces the Project with a new lifetime of the same name
// right after the controller has read it.
type recreateAfterRead struct {
	*fakeTerminationProjectStore
}

func (r *recreateAfterRead) GetProjectSnapshot(ctx context.Context, name string) (*ProjectSnapshot, error) {
	snap, err := r.fakeTerminationProjectStore.GetProjectSnapshot(ctx, name)
	if err == nil {
		r.mu.Lock()
		r.projects[name] = terminationProjectRecord{UID: "uid-2", Phase: v1.ProjectPhaseTerminated}
		r.mu.Unlock()
	}
	return snap, err
}
