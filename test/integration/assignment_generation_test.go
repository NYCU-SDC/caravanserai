//go:build e2e

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAssignmentGenerationFencing exercises CARA-83 against a real PostgreSQL:
// generation increments exactly once per grant, survives release, and fences
// stale Agent writes atomically — including the ABA case a bare Node name
// cannot detect.
func TestAssignmentGenerationFencing(t *testing.T) {
	const name = "e2e-assignment-generation"
	ctx := context.Background()
	sched := adapter.NewProjectStoreAdapter(suite.store)

	create := func(t *testing.T) {
		t.Helper()
		body := mustMarshal(t, v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
		})
		resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		drainBody(resp)
	}

	// Clean any prior run, then start fresh.
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	create(t)

	// A freshly created Project is NeverAssigned at generation zero.
	p := getProject(t, name)
	require.Equal(t, int64(0), p.Status.AssignmentGeneration, "new project starts at generation 0")
	require.Equal(t, v1.AssignmentHistoryNeverAssigned, p.Status.AssignmentHistory)
	uid := p.ObjectMeta.UID
	require.NotEmpty(t, uid)

	// First grant: Pending → Scheduled on node-a increments to 1 and turns Known.
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-a"))
	p = getProject(t, name)
	require.Equal(t, int64(1), p.Status.AssignmentGeneration, "first assignment increments to 1")
	require.Equal(t, v1.AssignmentHistoryKnown, p.Status.AssignmentHistory)
	require.Equal(t, "node-a", p.Status.NodeRef)

	// A scheduler retry that re-asserts the existing Scheduled assignment must
	// not increment the generation twice.
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-a"))
	require.Equal(t, int64(1), getProject(t, name).Status.AssignmentGeneration,
		"re-asserting an existing assignment must not bump the generation")

	// Releasing ownership preserves the counter.
	require.NoError(t, sched.SetProjectPending(ctx, name))
	p = getProject(t, name)
	require.Equal(t, int64(1), p.Status.AssignmentGeneration, "clearing nodeRef preserves the generation counter")
	require.Empty(t, p.Status.NodeRef)

	// ABA: move to node-b (gen 2), then back to node-a (gen 3). The Node name
	// returns to node-a but the generation never repeats.
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-b"))
	require.Equal(t, int64(2), getProject(t, name).Status.AssignmentGeneration)
	require.NoError(t, sched.SetProjectPending(ctx, name))
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-a"))
	current := getProject(t, name)
	require.Equal(t, int64(3), current.Status.AssignmentGeneration, "returning to node-a is a new generation, not a repeat")

	rvBefore := current.ObjectMeta.ResourceVersion

	// A generation-1 report from node-a — the old node-a grant — is rejected
	// even though the Node name matches the current node-a assignment.
	staleRef := store.AssignmentRef{UID: uid, NodeRef: "node-a", Generation: 1}
	err := suite.store.ReportProjectStatusFenced(ctx, name, staleRef, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	})
	require.ErrorIs(t, err, store.ErrStaleAssignment, "old node-a generation must be rejected (ABA)")

	// A report carrying a different UID is rejected even when node and
	// generation match the current assignment.
	wrongUID := store.AssignmentRef{UID: "some-other-uid", NodeRef: "node-a", Generation: 3}
	err = suite.store.ReportProjectStatusFenced(ctx, name, wrongUID, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	})
	require.ErrorIs(t, err, store.ErrStaleAssignment, "a different UID must be rejected")

	// The stale rejections changed nothing: same resourceVersion, same phase.
	after := getProject(t, name)
	assert.Equal(t, rvBefore, after.ObjectMeta.ResourceVersion, "a stale write must not bump resourceVersion")
	assert.Equal(t, v1.ProjectPhaseScheduled, after.Status.Phase, "a stale write must not change phase")

	// The current fence is accepted.
	currentRef := store.AssignmentRef{UID: uid, NodeRef: "node-a", Generation: 3}
	require.NoError(t, suite.store.ReportProjectStatusFenced(ctx, name, currentRef, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	}))
	assert.Equal(t, v1.ProjectPhaseRunning, getProject(t, name).Status.Phase)

	// Fenced Maintenance condition writes obey the same fence.
	require.ErrorIs(t,
		suite.store.PatchProjectConditionFenced(ctx, name, staleRef, v1.Condition{
			Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, Reason: "BackingUp",
		}),
		store.ErrStaleAssignment, "stale Maintenance patch must be rejected")
	require.NoError(t,
		suite.store.PatchProjectConditionFenced(ctx, name, currentRef, v1.Condition{
			Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, Reason: "BackingUp",
		}))
	require.ErrorIs(t,
		suite.store.ClearProjectConditionFenced(ctx, name, staleRef, v1.ConditionTypeMaintenance),
		store.ErrStaleAssignment, "stale Maintenance clear must be rejected")
	require.NoError(t,
		suite.store.ClearProjectConditionFenced(ctx, name, currentRef, v1.ConditionTypeMaintenance))

	// A correct repeated Running report remains a no-op: no resourceVersion bump.
	rv := getProject(t, name).ObjectMeta.ResourceVersion
	require.NoError(t, suite.store.ReportProjectStatusFenced(ctx, name, currentRef, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	}))
	assert.Equal(t, rv, getProject(t, name).ObjectMeta.ResourceVersion,
		"an unchanged repeated report must not bump resourceVersion")

	// Sentinels are distinct so callers can branch on them.
	assert.False(t, errors.Is(store.ErrStaleAssignment, store.ErrVersionConflict))

	// Cleanup.
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
}
