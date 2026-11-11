//go:build e2e

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setOverlap applies the stateless manifest createAssignedProject uses, with
// spec.overlap set.
func setOverlap(t *testing.T, name string, o v1.Overlap) {
	t.Helper()
	resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name, mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}, Overlap: o},
	}))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
}

func releaseOnGrace(t *testing.T, s *adapter.ProjectStoreAdapter, name string) bool {
	t.Helper()
	p := getProject(t, name)
	released, err := s.ReleaseAssignment(context.Background(), &controller.ProjectSnapshot{
		Name: name, UID: p.ObjectMeta.UID, Phase: p.Status.Phase, NodeRef: p.Status.NodeRef, AssignmentGeneration: p.Status.AssignmentGeneration,
	})
	require.NoError(t, err)
	return released
}

// TestOverlapForbidIsNotMovedOnTheGracePeriod: a Project that must not run
// twice stays on a NotReady Node when only the grace period has passed, and
// says why (ADR-0036).
func TestOverlapForbidIsNotMovedOnTheGracePeriod(t *testing.T) {
	const name = "e2e-overlap-forbid"
	on := adapter.NewProjectStoreAdapter(suite.store).WithGraceExpiredRelease(true)
	createAssignedProject(t, name, false)
	setOverlap(t, name, v1.OverlapForbid)
	assert.Equal(t, v1.OverlapForbid, getProject(t, name).Spec.Overlap)

	assert.False(t, releaseOnGrace(t, on, name))
	got := getProject(t, name)
	assert.Equal(t, "node-a", got.Status.NodeRef)
	assert.Equal(t, v1.ProjectPhaseRunning, got.Status.Phase)
	cond, ok := findCondition(got.Status.Conditions, v1.ConditionTypeTakeoverRequired)
	require.True(t, ok)
	assert.Equal(t, v1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "spec.overlap is Forbid")

	// Allowing overlap again takes effect on the next decision.
	setOverlap(t, name, v1.OverlapAllow)
	require.True(t, releaseOnGrace(t, on, name))
	got = getProject(t, name)
	assert.Equal(t, v1.ProjectPhasePending, got.Status.Phase)
	require.Len(t, got.Status.Recovery.CleanupObligations, 1)
	assert.Equal(t, v1.ReleaseByGraceExpired, got.Status.Recovery.CleanupObligations[0].ReleaseAuthorization)
}

// TestOverlapForbidStillMovesOnStopEvidence: Forbid only refuses moving
// without proof; once the old Node confirms the workload stopped, nothing can
// run twice and the Project moves as any other.
func TestOverlapForbidStillMovesOnStopEvidence(t *testing.T) {
	const name = "e2e-overlap-forbid-stopped"
	ctx := context.Background()
	on := adapter.NewProjectStoreAdapter(suite.store).WithGraceExpiredRelease(true)
	p := createAssignedProject(t, name, false)
	setOverlap(t, name, v1.OverlapForbid)

	ref := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-a", Generation: p.Status.AssignmentGeneration}
	require.NoError(t, suite.store.RecordWorkloadStopped(ctx, name, ref, nil, time.Now()))
	require.True(t, releaseOnGrace(t, on, name))
	got := getProject(t, name)
	require.Len(t, got.Status.Recovery.CleanupObligations, 1)
	assert.Equal(t, v1.ReleaseByWorkloadStopped, got.Status.Recovery.CleanupObligations[0].ReleaseAuthorization)
}

// TestOverlapForbidMessageOnlyWhenItDecides: with the grace-period release off,
// overlap changes nothing, and the condition does not blame it.
func TestOverlapForbidMessageOnlyWhenItDecides(t *testing.T) {
	const name = "e2e-overlap-forbid-off"
	off := adapter.NewProjectStoreAdapter(suite.store)
	createAssignedProject(t, name, false)
	setOverlap(t, name, v1.OverlapForbid)

	assert.False(t, releaseOnGrace(t, off, name))
	cond, ok := findCondition(getProject(t, name).Status.Conditions, v1.ConditionTypeTakeoverRequired)
	require.True(t, ok)
	assert.NotContains(t, cond.Message, "spec.overlap")
}
