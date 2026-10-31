//go:build e2e

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectRecoveryStatusRoundTrip exercises CARA-84's recovery status
// against a real PostgreSQL: every field survives the JSONB column and the API
// response, and reasserting identical evidence writes nothing.
func TestProjectRecoveryStatusRoundTrip(t *testing.T) {
	const name = "e2e-recovery-status"
	ctx := context.Background()

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	body := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	uid := getProject(t, name).ObjectMeta.UID

	attempt := time.Date(2026, 10, 3, 9, 0, 0, 123456789, time.UTC)
	healthy := attempt.Add(5 * time.Minute)
	previous := v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: 6}
	want := v1.ProjectRecoveryStatus{
		RescheduleAttempts:     2,
		LastAttemptAt:          &attempt,
		HealthySince:           &healthy,
		HealthySinceGeneration: 7,
		PreferredNode:          "node-b",
		WorkloadStopped:        &v1.WorkloadStoppedEvidence{Assignment: previous, ObservedAt: attempt.Add(-time.Minute)},
		CleanupObligations: []v1.CleanupObligation{{
			Assignment:           previous,
			ReleasedAt:           attempt,
			ReleaseAuthorization: v1.ReleaseByWorkloadStopped,
			ResourcesRemoved:     &v1.ResourcesRemovedEvidence{ObservedAt: attempt.Add(time.Minute)},
		}},
	}
	write := func(s *v1.ProjectStatus) error {
		s.Recovery = want.DeepCopy()
		return nil
	}

	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, write))
	stored := getProject(t, name)
	got := stored.Status.Recovery
	require.NotNil(t, got, "recovery status was dropped on the way through PostgreSQL")

	assert.Equal(t, want.RescheduleAttempts, got.RescheduleAttempts)
	assert.True(t, want.LastAttemptAt.Equal(*got.LastAttemptAt))
	assert.True(t, want.HealthySince.Equal(*got.HealthySince))
	assert.Equal(t, want.HealthySinceGeneration, got.HealthySinceGeneration)
	assert.Equal(t, want.PreferredNode, got.PreferredNode)
	assert.Equal(t, previous, got.WorkloadStopped.Assignment)
	assert.True(t, want.WorkloadStopped.ObservedAt.Equal(got.WorkloadStopped.ObservedAt))
	require.Len(t, got.CleanupObligations, 1)
	o := got.CleanupObligations[0]
	assert.Equal(t, previous, o.Assignment)
	assert.True(t, attempt.Equal(o.ReleasedAt))
	assert.Equal(t, v1.ReleaseByWorkloadStopped, o.ReleaseAuthorization)
	require.True(t, o.Resolved())
	assert.True(t, want.CleanupObligations[0].ResourcesRemoved.ObservedAt.Equal(o.ResourcesRemoved.ObservedAt))

	// The same evidence arriving again must not produce a new version.
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, write))
	assert.Equal(t, stored.ObjectMeta.ResourceVersion, getProject(t, name).ObjectMeta.ResourceVersion,
		"reasserting identical recovery evidence bumped the resource version")

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
}

// Status is server-owned. A client that sends any of it on create — a phase and
// Node that skip the Scheduler, an assignment history that disables immediate
// deletion, forged stop evidence — must not have it stored, because
// controllers would later act on it as if the server had recorded it.
func TestCreateProjectResetsClientStatus(t *testing.T) {
	const name = "e2e-status-injection"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))

	forged := v1.AssignmentIdentity{ProjectUID: "forged", NodeName: "node-x", AssignmentGeneration: 99}
	body := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
		Status: v1.ProjectStatus{
			Phase:                v1.ProjectPhaseRunning,
			NodeRef:              "node-x",
			AssignmentGeneration: 99,
			AssignmentHistory:    v1.AssignmentHistoryKnown,
			FailedNodes:          []string{"node-y"},
			Conditions: []v1.Condition{{
				Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: "Forged", LastTransitionTime: time.Now(),
			}},
			Recovery: &v1.ProjectRecoveryStatus{
				RescheduleAttempts: 42,
				PreferredNode:      "node-x",
				WorkloadStopped:    &v1.WorkloadStoppedEvidence{Assignment: forged, ObservedAt: time.Now()},
				CleanupObligations: []v1.CleanupObligation{{
					Assignment: forged, ReleasedAt: time.Now(), ReleaseAuthorization: v1.ReleaseByOperatorOverride,
					ResourcesRemoved: &v1.ResourcesRemovedEvidence{ObservedAt: time.Now()},
				}},
			},
		},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created v1.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	drainBody(resp)

	assert.Equal(t, v1.InitialProjectStatus(), created.Status, "the create response echoed client-supplied status")
	assert.Equal(t, v1.InitialProjectStatus(), getProject(t, name).Status, "client-supplied status was persisted")

	// The forged Running phase must not reach the phase column either, or
	// phase-filtered reads would see a Project the Scheduler never placed.
	resp = doRequest(t, http.MethodGet, "/api/v1/projects?phase=Running", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var running v1.ProjectList
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&running))
	drainBody(resp)
	for _, p := range running.Items {
		assert.NotEqual(t, name, p.Name, "the forged Running phase was stored in the phase column")
	}

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
}
