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

// TestProjectStatusPatch verifies that PATCH /api/v1/projects/{name}/status
// rejects unrecognised phase values with 400 and accepts valid ones with 204.
func TestProjectStatusPatch(t *testing.T) {
	const projectName = "e2e-phase-validation"

	// ── Setup: create a project so we have something to patch ────────────────

	createBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{
				{Name: "web", Image: "nginx:latest"},
			},
		},
	})

	resp := doRequest(t, http.MethodPost, "/api/v1/projects", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create project: expected 201")
	drainBody(resp)

	// ── 1. Valid phase → 204 ────────────────────────────────────────────────

	patchBody := mustMarshal(t, map[string]string{"phase": "Running"})
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "valid phase: expected 204")
	drainBody(resp)

	// ── 2. Invalid phase → 400 ──────────────────────────────────────────────

	patchBody = mustMarshal(t, map[string]string{"phase": "ClearlyBogusPhase"})
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "invalid phase: expected 400")

	var errResp struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&errResp))
	_ = resp.Body.Close()

	assert.Contains(t, errResp.Error, "ClearlyBogusPhase", "error message should mention the invalid value")
	assert.Contains(t, errResp.Error, "Pending", "error message should list valid phases")

	// ── 3. Empty phase → 400 ────────────────────────────────────────────────

	patchBody = mustMarshal(t, map[string]string{"phase": ""})
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "empty phase: expected 400")
	drainBody(resp)

	// ── Cleanup ─────────────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName, nil)
	assert.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, resp.StatusCode, "delete project")
	drainBody(resp)
}
