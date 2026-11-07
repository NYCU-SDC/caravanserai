//go:build e2e

package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// placeInPhase assigns an existing Pending Project to node-a through the
// Scheduler and then writes phase directly, past the transition guards. It is a
// fixture for tests about something other than how a Project reaches a phase;
// the guards themselves are exercised through the endpoints below.
func placeInPhase(t *testing.T, name string, phase v1.ProjectPhase, reason, message string) {
	t.Helper()
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(suite.store)
	require.NoError(t, projects.SetProjectScheduled(ctx, name, "node-a"))
	if phase != v1.ProjectPhaseScheduled {
		require.NoError(t, projects.SetProjectPhase(ctx, name, phase, reason, message))
	}
}

// CARA-81 stage 4: the Agent status endpoint and the delete API follow the
// lifecycle transition table against the real store. A refused transition
// leaves the record exactly as it was, version included.
func TestLifecycleTransitionGuards(t *testing.T) {
	const name = "e2e-transition-guards"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	report := func(phase v1.ProjectPhase) int {
		t.Helper()
		resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+name+"/status",
			[]byte(fmt.Sprintf(`{"phase":%q,"reason":"Test","uid":%q,"nodeRef":"node-a","assignmentGeneration":%d}`,
				phase, uid, gen)))
		defer drainBody(resp)
		return resp.StatusCode
	}
	deleteProject := func() int {
		t.Helper()
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
		defer drainBody(resp)
		return resp.StatusCode
	}
	// refused asserts that a request is answered 409 without touching the row.
	refused := func(t *testing.T, what string, do func() int) {
		t.Helper()
		before := getProject(t, name)
		assert.Equal(t, http.StatusConflict, do(), what)
		after := getProject(t, name)
		assert.Equal(t, before.Status.Phase, after.Status.Phase, "%s changed the phase", what)
		assert.Equal(t, before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion, "%s bumped the version", what)
	}

	require.Equal(t, http.StatusAccepted, deleteProject())
	require.Equal(t, v1.ProjectPhaseTerminating, getProject(t, name).Status.Phase)

	t.Run("a late Running or Failed report does not undo the deletion", func(t *testing.T) {
		refused(t, "Running while Terminating", func() int { return report(v1.ProjectPhaseRunning) })
		refused(t, "Failed while Terminating", func() int { return report(v1.ProjectPhaseFailed) })
	})

	t.Run("Terminated requires ResourcesRemoved for the reporting assignment", func(t *testing.T) {
		refused(t, "Terminated before ResourcesRemoved", func() int { return report(v1.ProjectPhaseTerminated) })

		require.Equal(t, http.StatusNoContent,
			putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen)))
		require.Equal(t, http.StatusNoContent, report(v1.ProjectPhaseTerminated))
		assert.Equal(t, v1.ProjectPhaseTerminated, getProject(t, name).Status.Phase)
	})

	t.Run("Terminated is terminal", func(t *testing.T) {
		refused(t, "Running after Terminated", func() int { return report(v1.ProjectPhaseRunning) })

		before := getProject(t, name)
		assert.Equal(t, http.StatusAccepted, deleteProject(), "a repeated delete is still accepted")
		after := getProject(t, name)
		assert.Equal(t, v1.ProjectPhaseTerminated, after.Status.Phase, "the delete reopened a finished deletion")
		assert.Equal(t, before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	})
}

// S9: the Scheduler's assignment and its Phase condition are one write, so the
// condition never still describes the Pending the Project has left.
func TestSchedulingWritesThePhaseCondition(t *testing.T) {
	const name = "e2e-scheduled-condition"
	createAssigned(t, name, "node-a", v1.ProjectPhaseScheduled)

	p := getProject(t, name)
	require.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase)
	var reason string
	for _, c := range p.Status.Conditions {
		if c.Type == v1.ConditionTypePhase {
			reason = c.Reason
		}
	}
	assert.Equal(t, "Scheduled", reason)
}
