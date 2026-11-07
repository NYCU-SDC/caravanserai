//go:build e2e

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func snapshotOf(p v1.Project) *controller.ProjectSnapshot {
	return &controller.ProjectSnapshot{
		Name:                 p.Name,
		UID:                  p.ObjectMeta.UID,
		Phase:                p.Status.Phase,
		NodeRef:              p.Status.NodeRef,
		AssignmentGeneration: p.Status.AssignmentGeneration,
	}
}

func hasProjectCondition(p v1.Project, t v1.ConditionType) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}

// CARA-84: an assignment is released only with current-generation
// WorkloadStopped. Without it the Project keeps its nodeRef and is marked
// TakeoverRequired; with it, nodeRef is cleared and the cleanup obligation is
// recorded in the same write.
func TestReleaseGate(t *testing.T) {
	const name = "e2e-release-gate"
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(suite.store)
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	snap := snapshotOf(getProject(t, name))
	assignment := v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}

	t.Run("refused without evidence", func(t *testing.T) {
		released, err := projects.ReleaseAssignment(ctx, snap)
		require.NoError(t, err)
		assert.False(t, released)

		p := getProject(t, name)
		assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase)
		assert.Equal(t, "node-a", p.Status.NodeRef)
		assert.True(t, hasProjectCondition(p, v1.ConditionTypeTakeoverRequired))
		if p.Status.Recovery != nil {
			assert.Empty(t, p.Status.Recovery.CleanupObligations, "a refused release recorded an obligation")
		}

		// The next reconcile refuses again without writing.
		released, err = projects.ReleaseAssignment(ctx, snap)
		require.NoError(t, err)
		assert.False(t, released)
		assert.Equal(t, p.ObjectMeta.ResourceVersion, getProject(t, name).ObjectMeta.ResourceVersion,
			"re-marking TakeoverRequired wrote")
	})

	t.Run("evidence for another generation does not authorize", func(t *testing.T) {
		older := assignment
		older.AssignmentGeneration--
		require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
			s.EnsureRecovery().WorkloadStopped = &v1.WorkloadStoppedEvidence{Assignment: older}
			return nil
		}))
		t.Cleanup(func() {
			require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
				s.Recovery.WorkloadStopped = nil
				return nil
			}))
		})

		released, err := projects.ReleaseAssignment(ctx, snap)
		require.NoError(t, err)
		assert.False(t, released)
		assert.Equal(t, "node-a", getProject(t, name).Status.NodeRef)
	})

	t.Run("a stale snapshot is refused without a write", func(t *testing.T) {
		before := getProject(t, name).ObjectMeta.ResourceVersion
		for _, stale := range []controller.ProjectSnapshot{
			{Name: name, UID: uid, NodeRef: "node-a", AssignmentGeneration: gen - 1},
			{Name: name, UID: uid, NodeRef: "node-b", AssignmentGeneration: gen},
			{Name: name, UID: "other-uid", NodeRef: "node-a", AssignmentGeneration: gen},
		} {
			_, err := projects.ReleaseAssignment(ctx, &stale)
			assert.ErrorIs(t, err, store.ErrStaleAssignment, "%+v", stale)
		}
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion)
	})

	t.Run("released with current evidence", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent,
			putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))

		released, err := projects.ReleaseAssignment(ctx, snap)
		require.NoError(t, err)
		require.True(t, released)

		p := getProject(t, name)
		assert.Equal(t, v1.ProjectPhasePending, p.Status.Phase)
		assert.Empty(t, p.Status.NodeRef)
		assert.Equal(t, gen, p.Status.AssignmentGeneration, "release changed the generation counter")
		assert.False(t, hasProjectCondition(p, v1.ConditionTypeTakeoverRequired), "release kept TakeoverRequired")
		assert.NotNil(t, p.Status.Recovery.WorkloadStopped, "release consumed the no-start latch")

		require.Len(t, p.Status.Recovery.CleanupObligations, 1)
		o := p.Status.Recovery.CleanupObligations[0]
		assert.Equal(t, assignment, o.Assignment)
		assert.Equal(t, v1.ReleaseByWorkloadStopped, o.ReleaseAuthorization)
		assert.False(t, o.ReleasedAt.IsZero())
		assert.False(t, o.Resolved())

		// The snapshot is now stale: the assignment it names is gone.
		_, err = projects.ReleaseAssignment(ctx, snap)
		assert.ErrorIs(t, err, store.ErrStaleAssignment)
	})
}

// A Terminating Project is never released to another Node, even with stop
// evidence: its resources are to be removed, not handed over.
func TestReleaseGateRefusesTerminating(t *testing.T) {
	const name = "e2e-release-terminating"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	snap := snapshotOf(getProject(t, name))

	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
	resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
	drainBody(resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	_, err := adapter.NewProjectStoreAdapter(suite.store).ReleaseAssignment(ctx, snap)
	assert.ErrorIs(t, err, store.ErrConflictState)
	assert.Equal(t, "node-a", getProject(t, name).Status.NodeRef)
}

// CARA-84: no assignment is committed while an obligation lacks both
// ResourcesRemoved and a cross-Node-eligible authorization — not even back to
// the Node that owes it.
func TestAssignmentBlockedByCleanupObligation(t *testing.T) {
	const name = "e2e-assign-blocked"
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(suite.store)
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
	released, err := projects.ReleaseAssignment(ctx, snapshotOf(getProject(t, name)))
	require.NoError(t, err)
	require.True(t, released)

	before := getProject(t, name).ObjectMeta.ResourceVersion
	for _, node := range []string{"node-b", "node-a"} {
		err := projects.SetProjectScheduled(ctx, name, node)
		assert.ErrorIs(t, err, store.ErrConflictState, "assigned to %s with cleanup outstanding", node)
	}
	assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion, "a refused assignment wrote")

	// An unresolved obligation released by an audited override does not
	// block; only the WorkloadStopped one does, until its cleanup is reported.
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
		s.Recovery.AddObligation(v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-z", AssignmentGeneration: gen - 1},
			s.Recovery.CleanupObligations[0].ReleasedAt, v1.ReleaseByOperatorOverride)
		return nil
	}))
	assert.ErrorIs(t, projects.SetProjectScheduled(ctx, name, "node-b"), store.ErrConflictState)

	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen)))
	require.NoError(t, projects.SetProjectScheduled(ctx, name, "node-b"))

	p := getProject(t, name)
	assert.Equal(t, "node-b", p.Status.NodeRef)
	assert.Equal(t, gen+1, p.Status.AssignmentGeneration)
	assert.Len(t, p.Status.Recovery.CleanupObligations, 2, "assignment dropped an obligation")
}

// CARA-84: entering Terminating records the current assignment's cleanup
// obligation in the same write; a Project with no current assignment adds
// none.
func TestTerminatingRecordsDeletionObligation(t *testing.T) {
	del := func(t *testing.T, name string) {
		t.Helper()
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
		drainBody(resp)
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
	}

	t.Run("assigned", func(t *testing.T) {
		const name = "e2e-terminating-assigned"
		uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

		del(t, name)
		p := getProject(t, name)
		assert.Equal(t, v1.ProjectPhaseTerminating, p.Status.Phase)
		assert.Equal(t, "node-a", p.Status.NodeRef, "Terminating gave up the assignment the Agent tears down under")
		require.Len(t, p.Status.Recovery.CleanupObligations, 1)
		o := p.Status.Recovery.CleanupObligations[0]
		assert.Equal(t, v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}, o.Assignment)
		assert.Equal(t, v1.ReleaseByDeletion, o.ReleaseAuthorization)
		assert.False(t, o.Resolved())

		// A repeated delete changes nothing.
		del(t, name)
		assert.Equal(t, p.ObjectMeta.ResourceVersion, getProject(t, name).ObjectMeta.ResourceVersion)
	})

	t.Run("released earlier", func(t *testing.T) {
		const name = "e2e-terminating-released"
		createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
		testhelper.ReleaseCleanly(t, suite.store, name)

		del(t, name)
		p := getProject(t, name)
		assert.Equal(t, v1.ProjectPhaseTerminating, p.Status.Phase)
		assert.Len(t, p.Status.Recovery.CleanupObligations, 1, "only the release's obligation; no current assignment to add")
	})
}

// SetProjectScheduled acts on a phase the scheduler read earlier. Only a
// Pending Project may receive a new assignment, and only the same Node may be
// re-asserted on a Scheduled one; the check runs on the snapshot the swap is
// versioned on.
func TestSetProjectScheduledRequiresPending(t *testing.T) {
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(suite.store)

	t.Run("a deletion after the scheduler's read is not undone", func(t *testing.T) {
		const name = "e2e-schedule-after-delete"
		createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
		testhelper.ReleaseCleanly(t, suite.store, name)
		require.Equal(t, v1.ProjectPhasePending, getProject(t, name).Status.Phase) // what the scheduler read

		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
		drainBody(resp)
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		before := getProject(t, name)

		assert.ErrorIs(t, projects.SetProjectScheduled(ctx, name, "node-b"), store.ErrConflictState)
		after := getProject(t, name)
		assert.Equal(t, v1.ProjectPhaseTerminating, after.Status.Phase, "a late assignment undid the deletion")
		assert.Equal(t, before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	})

	t.Run("a Scheduled project is not moved to another node", func(t *testing.T) {
		const name = "e2e-schedule-move"
		_, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseScheduled)
		before := getProject(t, name)

		assert.ErrorIs(t, projects.SetProjectScheduled(ctx, name, "node-b"), store.ErrConflictState)
		after := getProject(t, name)
		assert.Equal(t, "node-a", after.Status.NodeRef)
		assert.Equal(t, gen, after.Status.AssignmentGeneration)
		assert.Equal(t, before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	})

	t.Run("re-asserting the same node is a no-op", func(t *testing.T) {
		const name = "e2e-schedule-reassert"
		_, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseScheduled)
		before := getProject(t, name)

		require.NoError(t, projects.SetProjectScheduled(ctx, name, "node-a"))
		after := getProject(t, name)
		assert.Equal(t, gen, after.Status.AssignmentGeneration)
		assert.Equal(t, before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	})

	t.Run("a running or failed project is not reassigned", func(t *testing.T) {
		for _, phase := range []v1.ProjectPhase{v1.ProjectPhaseRunning, v1.ProjectPhaseFailed} {
			name := "e2e-schedule-" + strings.ToLower(string(phase))
			createAssigned(t, name, "node-a", phase)
			assert.ErrorIs(t, projects.SetProjectScheduled(ctx, name, "node-a"), store.ErrConflictState, "%s", phase)
			assert.Equal(t, phase, getProject(t, name).Status.Phase)
		}
	})
}
