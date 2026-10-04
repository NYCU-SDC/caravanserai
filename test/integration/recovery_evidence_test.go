//go:build e2e

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evidenceBody is the fence an Agent presents to the evidence endpoints.
func evidenceBody(uid, nodeRef string, generation int64) []byte {
	return []byte(fmt.Sprintf(`{"uid":%q,"nodeRef":%q,"assignmentGeneration":%d}`, uid, nodeRef, generation))
}

func putEvidence(t *testing.T, name, kind string, body []byte) int {
	t.Helper()
	resp := doRequest(t, http.MethodPut, "/api/v1/projects/"+name+"/recovery/"+kind, body)
	defer drainBody(resp)
	return resp.StatusCode
}

// createAssigned creates a fresh Project, assigns it to node, and moves it to
// phase. It returns the Project's UID and assignment generation.
func createAssigned(t *testing.T, name, node string, phase v1.ProjectPhase) (string, int64) {
	t.Helper()
	ctx := context.Background()
	sched := adapter.NewProjectStoreAdapter(suite.store)

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	require.NoError(t, sched.SetProjectScheduled(ctx, name, node))
	if phase != v1.ProjectPhaseScheduled {
		require.NoError(t, sched.SetProjectPhase(ctx, name, phase, "Test", ""))
	}
	p := getProject(t, name)
	t.Cleanup(func() {
		drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	})
	return p.ObjectMeta.UID, p.Status.AssignmentGeneration
}

// CARA-84: only the currently assigned Agent may record WorkloadStopped, the
// evidence identity is the fence, and anything stale is refused without a
// write.
func TestWorkloadStoppedEvidence(t *testing.T) {
	const name = "e2e-workload-stopped"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	t.Run("refused without a fence even in non-strict mode", func(t *testing.T) {
		before := getProject(t, name).ObjectMeta.ResourceVersion
		assert.Equal(t, http.StatusConflict, putEvidence(t, name, "workload-stopped", []byte(`{}`)))
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion)
	})

	t.Run("refused for a stale fence", func(t *testing.T) {
		before := getProject(t, name).ObjectMeta.ResourceVersion
		for _, body := range [][]byte{
			evidenceBody(uid, "node-a", gen-1),
			evidenceBody(uid, "node-b", gen),
			evidenceBody("other-uid", "node-a", gen),
		} {
			assert.Equal(t, http.StatusConflict, putEvidence(t, name, "workload-stopped", body))
		}
		p := getProject(t, name)
		assert.Equal(t, before, p.ObjectMeta.ResourceVersion, "a refused write bumped the version")
		assert.Nil(t, p.Status.Recovery)
	})

	t.Run("recorded from the current assignment", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
		ws := getProject(t, name).Status.Recovery.WorkloadStopped
		require.NotNil(t, ws)
		assert.Equal(t, v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}, ws.Assignment)
		assert.False(t, ws.ObservedAt.IsZero())
	})

	t.Run("reassertion is a no-op that keeps the first observation", func(t *testing.T) {
		first := getProject(t, name)
		time.Sleep(10 * time.Millisecond)
		require.Equal(t, http.StatusNoContent, putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
		again := getProject(t, name)
		assert.Equal(t, first.ObjectMeta.ResourceVersion, again.ObjectMeta.ResourceVersion)
		assert.True(t, first.Status.Recovery.WorkloadStopped.ObservedAt.Equal(again.Status.Recovery.WorkloadStopped.ObservedAt))
	})
}

func TestWorkloadStoppedEvidencePhaseGate(t *testing.T) {
	ctx := context.Background()
	sched := adapter.NewProjectStoreAdapter(suite.store)

	for _, phase := range []v1.ProjectPhase{v1.ProjectPhaseScheduled, v1.ProjectPhaseRunning, v1.ProjectPhaseFailed, v1.ProjectPhaseTerminating} {
		t.Run("accepted while "+string(phase), func(t *testing.T) {
			name := "e2e-ws-phase-" + strings.ToLower(string(phase))
			uid, gen := createAssigned(t, name, "node-a", phase)
			assert.Equal(t, http.StatusNoContent, putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
		})
	}

	t.Run("refused while Terminated", func(t *testing.T) {
		const name = "e2e-ws-phase-terminated"
		uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
		require.NoError(t, sched.SetProjectPhase(ctx, name, v1.ProjectPhaseTerminated, "Test", ""))
		before := getProject(t, name).ObjectMeta.ResourceVersion
		assert.Equal(t, http.StatusConflict, putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion)
	})
}

// CARA-84: ResourcesRemoved is accepted from the current assignment or the
// durable PreviousAssignment, and from nobody else.
func TestResourcesRemovedEvidence(t *testing.T) {
	ctx := context.Background()

	t.Run("recorded from the current assignment", func(t *testing.T) {
		const name = "e2e-resources-removed-current"
		uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
		require.Equal(t, http.StatusNoContent, putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen)))
		rr := getProject(t, name).Status.Recovery.ResourcesRemoved
		require.NotNil(t, rr)
		assert.Equal(t, v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}, rr.Assignment)
	})

	t.Run("recorded from the durable previous assignment, refused from anyone else", func(t *testing.T) {
		const name = "e2e-resources-removed-previous"
		uid, gen := createAssigned(t, name, "node-b", v1.ProjectPhaseRunning)
		previous := v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen - 1}
		require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
			s.Recovery = &v1.ProjectRecoveryStatus{PreviousAssignment: &previous}
			return nil
		}))

		before := getProject(t, name).ObjectMeta.ResourceVersion
		for _, body := range [][]byte{
			evidenceBody(uid, "node-a", gen-2),         // older than the previous assignment
			evidenceBody(uid, "node-c", gen-1),         // right generation, wrong Node
			evidenceBody("other-uid", "node-a", gen-1), // another lifetime with the same Node and generation
		} {
			assert.Equal(t, http.StatusConflict, putEvidence(t, name, "resources-removed", body))
		}
		assert.Equal(t, before, getProject(t, name).ObjectMeta.ResourceVersion, "a refused write bumped the version")

		require.Equal(t, http.StatusNoContent, putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen-1)))
		got := getProject(t, name)
		assert.Equal(t, previous, got.Status.Recovery.ResourcesRemoved.Assignment)

		// Reassertion is a no-op.
		require.Equal(t, http.StatusNoContent, putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen-1)))
		assert.Equal(t, got.ObjectMeta.ResourceVersion, getProject(t, name).ObjectMeta.ResourceVersion)
	})
}

// CARA-84: an ordinary Agent status report carries no authority over recovery
// state, even when its body names recovery fields.
func TestAgentStatusReportCannotWriteRecovery(t *testing.T) {
	const name = "e2e-status-report-recovery"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	body := fmt.Sprintf(`{"phase":"Running","uid":%q,"nodeRef":"node-a","assignmentGeneration":%d,`+
		`"recovery":{"rescheduleAttempts":9,"preferredNode":"node-x",`+
		`"workloadStopped":{"assignment":{"projectUID":%q,"nodeName":"node-a","assignmentGeneration":%d},"observedAt":"2026-10-04T00:00:00Z"}}}`,
		uid, gen, uid, gen)
	resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+name+"/status", []byte(body))
	drainBody(resp)
	require.Less(t, resp.StatusCode, 300)

	assert.Nil(t, getProject(t, name).Status.Recovery, "a status report wrote recovery state")
}

// CARA-84: a committed new assignment consumes the old generation's no-start
// latch and healthy window in the same write; re-asserting the existing
// assignment does not. A reported failure ends the healthy window.
func TestNewAssignmentClearsLatchAndHealthyWindow(t *testing.T) {
	ctx := context.Background()
	sched := adapter.NewProjectStoreAdapter(suite.store)
	const name = "e2e-assignment-clears-latch"
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	healthy := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	setHealthy := func() {
		require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
			if s.Recovery == nil {
				s.Recovery = &v1.ProjectRecoveryStatus{}
			}
			s.Recovery.HealthySince = &healthy
			s.Recovery.HealthySinceGeneration = s.AssignmentGeneration
			return nil
		}))
	}
	setHealthy()
	require.Equal(t, http.StatusNoContent, putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))

	// Re-asserting the current Scheduled assignment is not a new assignment.
	require.NoError(t, sched.SetProjectPhase(ctx, name, v1.ProjectPhaseScheduled, "Test", ""))
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-a"))
	r := getProject(t, name).Status.Recovery
	require.NotNil(t, r.WorkloadStopped, "re-asserting an assignment cleared the latch")
	require.NotNil(t, r.HealthySince)

	// A real new assignment clears both.
	require.NoError(t, sched.SetProjectPending(ctx, name))
	require.NoError(t, sched.SetProjectScheduled(ctx, name, "node-b"))
	p := getProject(t, name)
	assert.Equal(t, gen+1, p.Status.AssignmentGeneration)
	assert.Nil(t, p.Status.Recovery.WorkloadStopped, "new assignment kept the old no-start latch")
	assert.Nil(t, p.Status.Recovery.HealthySince, "new assignment kept the old healthy window")

	// A reported failure ends the healthy window, through both the controller
	// path and the Agent report path.
	setHealthy()
	require.NoError(t, sched.SetProjectPhase(ctx, name, v1.ProjectPhaseFailed, "Test", ""))
	assert.Nil(t, getProject(t, name).Status.Recovery.HealthySince)

	require.NoError(t, sched.SetProjectPhase(ctx, name, v1.ProjectPhaseRunning, "Test", ""))
	setHealthy()
	resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+name+"/status",
		[]byte(fmt.Sprintf(`{"phase":"Failed","uid":%q,"nodeRef":"node-b","assignmentGeneration":%d}`, uid, gen+1)))
	drainBody(resp)
	require.Less(t, resp.StatusCode, 300)
	assert.Nil(t, getProject(t, name).Status.Recovery.HealthySince)
}
