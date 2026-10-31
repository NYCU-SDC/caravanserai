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

	t.Run("a Terminating project its node still holds is left to that node", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminating, NodeRef: "node-a"}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		assert.Empty(t, ps.FinalizeCalls, "the assigned node reports Terminated; the server does not finalize past it")
	})

	t.Run("a Terminating project with no current assignment is finalized by the server", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminating}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		res, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.False(t, res.Requeue)
		require.Equal(t, []finalizeCall{{Name: "my-app", UID: "uid-1"}}, ps.FinalizeCalls)
	})

	t.Run("a Running project is a no-op", func(t *testing.T) {
		ps := newFakeTerminationProjectStore()
		ps.projects["my-app"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseRunning, NodeRef: "node-a"}
		ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

		_, err := ctrl.Reconcile(context.Background(), "my-app")
		require.NoError(t, err)
		assert.Empty(t, ps.FinalizeCalls)
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

func TestProjectTerminationResyncCoversUnassignedTerminating(t *testing.T) {
	ps := newFakeTerminationProjectStore()
	ps.projects["done"] = terminationProjectRecord{UID: "uid-1", Phase: v1.ProjectPhaseTerminated}
	ps.projects["unassigned"] = terminationProjectRecord{UID: "uid-2", Phase: v1.ProjectPhaseTerminating}
	ps.projects["running"] = terminationProjectRecord{UID: "uid-3", Phase: v1.ProjectPhaseRunning}
	ctrl := NewProjectTerminationController(zap.NewNop(), ps, nil)

	var enqueued []string
	ctrl.resyncTerminated(context.Background(), func(name string) { enqueued = append(enqueued, name) })

	assert.ElementsMatch(t, []string{"done", "unassigned"}, enqueued)
}
