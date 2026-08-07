//go:build e2e

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getProject fetches a Project through the API.
func getProject(t *testing.T, name string) v1.Project {
	t.Helper()
	resp := doRequest(t, http.MethodGet, "/api/v1/projects/"+name, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer drainBody(resp)

	var p v1.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	return p
}

func findCondition(conditions []v1.Condition, condType v1.ConditionType) (v1.Condition, bool) {
	for _, c := range conditions {
		if c.Type == condType {
			return c, true
		}
	}
	return v1.Condition{}, false
}

// TestProjectConditionPatchIsolatesPhase is the guarantee CARA-59 depends on:
// an agent must be able to advertise Maintenance during a backup while the
// Project stays Running. If patching a condition disturbed phase, the backup
// would unlock the apply and delete paths that are deliberately closed for a
// Running Project.
func TestProjectConditionPatchIsolatesPhase(t *testing.T) {
	const projectName = "e2e-condition-isolation"

	createBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest"}},
		},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	t.Cleanup(func() {
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName+"?force=true", nil)
		drainBody(resp)
	})

	// Move the Project to Running with a Phase condition, mimicking a healthy
	// agent report.
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status",
		mustMarshal(t, map[string]string{
			"phase": "Running", "reason": "ContainersRunning", "message": "All containers running",
		}))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)

	before := getProject(t, projectName)
	require.Equal(t, v1.ProjectPhaseRunning, before.Status.Phase)
	_, hadPhaseCondition := findCondition(before.Status.Conditions, v1.ConditionTypePhase)
	require.True(t, hadPhaseCondition, "setup should have written a Phase condition")

	// ── Set Maintenance ─────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodPatch,
		"/api/v1/projects/"+projectName+"/conditions/Maintenance",
		mustMarshal(t, map[string]string{
			"status": "True", "reason": "BackingUp", "message": "Containers stopped for backup",
		}))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)

	during := getProject(t, projectName)

	assert.Equal(t, v1.ProjectPhaseRunning, during.Status.Phase,
		"patching a condition must not disturb phase")

	maintenance, ok := findCondition(during.Status.Conditions, v1.ConditionTypeMaintenance)
	require.True(t, ok, "Maintenance condition should be present")
	assert.Equal(t, v1.ConditionTrue, maintenance.Status)
	assert.Equal(t, "BackingUp", maintenance.Reason)
	assert.False(t, maintenance.LastTransitionTime.IsZero())

	// The unrelated Phase condition must survive: the merge replaces one
	// entry rather than rewriting the whole conditions array.
	phaseCond, ok := findCondition(during.Status.Conditions, v1.ConditionTypePhase)
	require.True(t, ok, "the pre-existing Phase condition must survive a Maintenance patch")
	assert.Equal(t, "ContainersRunning", phaseCond.Reason)

	assert.True(t, v1.IsMaintenanceActive(during.Status.Conditions, maintenance.LastTransitionTime))

	// ── Re-assert the same condition ────────────────────────────────────────

	resp = doRequest(t, http.MethodPatch,
		"/api/v1/projects/"+projectName+"/conditions/Maintenance",
		mustMarshal(t, map[string]string{
			"status": "True", "reason": "BackingUp", "message": "Containers stopped for backup",
		}))
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "re-asserting a condition is not an error")
	drainBody(resp)

	// ── Clear Maintenance ───────────────────────────────────────────────────

	resp = doRequest(t, http.MethodDelete,
		"/api/v1/projects/"+projectName+"/conditions/Maintenance", nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)

	after := getProject(t, projectName)

	assert.Equal(t, v1.ProjectPhaseRunning, after.Status.Phase,
		"clearing a condition must not disturb phase either")
	_, stillThere := findCondition(after.Status.Conditions, v1.ConditionTypeMaintenance)
	assert.False(t, stillThere, "Maintenance should be gone")

	phaseCond, ok = findCondition(after.Status.Conditions, v1.ConditionTypePhase)
	require.True(t, ok, "the Phase condition must survive the clear")
	assert.Equal(t, "ContainersRunning", phaseCond.Reason)

	// Clearing an absent condition is a no-op, not an error — the backup's
	// deferred cleanup may run twice.
	resp = doRequest(t, http.MethodDelete,
		"/api/v1/projects/"+projectName+"/conditions/Maintenance", nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)
}

// TestProjectConditionRejectsControllerOwnedTypes verifies the endpoint only
// accepts conditions agents own. Phase, NotReadyAt and TerminatingAt drive
// scheduling decisions, so a node must not be able to forge them.
func TestProjectConditionRejectsControllerOwnedTypes(t *testing.T) {
	const projectName = "e2e-condition-authz"

	createBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest"}},
		},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	t.Cleanup(func() {
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName+"?force=true", nil)
		drainBody(resp)
	})

	for _, condType := range []string{"Phase", "NotReadyAt", "TerminatingAt", "Ready"} {
		t.Run(condType, func(t *testing.T) {
			resp := doRequest(t, http.MethodPatch,
				"/api/v1/projects/"+projectName+"/conditions/"+condType,
				mustMarshal(t, map[string]string{"status": "True", "reason": "Forged"}))
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
				"%s is controller-owned and must not be agent-writable", condType)
			drainBody(resp)

			resp = doRequest(t, http.MethodDelete,
				"/api/v1/projects/"+projectName+"/conditions/"+condType, nil)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			drainBody(resp)
		})
	}
}

// TestProjectConditionRejectsInvalidStatus verifies status validation.
func TestProjectConditionRejectsInvalidStatus(t *testing.T) {
	const projectName = "e2e-condition-status"

	createBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest"}},
		},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	t.Cleanup(func() {
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName+"?force=true", nil)
		drainBody(resp)
	})

	resp = doRequest(t, http.MethodPatch,
		"/api/v1/projects/"+projectName+"/conditions/Maintenance",
		mustMarshal(t, map[string]string{"status": "Yes", "reason": "BackingUp"}))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	drainBody(resp)
}

// TestProjectConditionOnMissingProject verifies a 404 rather than a silent
// success when the Project has been deleted mid-backup.
func TestProjectConditionOnMissingProject(t *testing.T) {
	resp := doRequest(t, http.MethodPatch,
		"/api/v1/projects/e2e-does-not-exist/conditions/Maintenance",
		mustMarshal(t, map[string]string{"status": "True", "reason": "BackingUp"}))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	drainBody(resp)
}
