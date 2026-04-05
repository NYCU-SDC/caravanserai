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

	type testCase struct {
		name       string
		phase      string
		wantStatus int
		validate   func(t *testing.T, resp *http.Response)
	}

	testCases := []testCase{
		{
			name:       "Valid phase Running returns 204",
			phase:      "Running",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "Valid phase Failed returns 204",
			phase:      "Failed",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "Valid phase Terminated returns 204",
			phase:      "Terminated",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "Invalid phase returns 400 with descriptive error",
			phase:      "ClearlyBogusPhase",
			wantStatus: http.StatusBadRequest,
			validate: func(t *testing.T, resp *http.Response) {
				var errResp struct {
					Error string `json:"error"`
				}
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&errResp))

				assert.Contains(t, errResp.Error, "ClearlyBogusPhase",
					"error message should mention the invalid value")
				assert.Contains(t, errResp.Error, "Pending",
					"error message should list valid phases")
			},
		},
		{
			name:       "Another invalid phase returns 400",
			phase:      "NotAPhase",
			wantStatus: http.StatusBadRequest,
			validate: func(t *testing.T, resp *http.Response) {
				var errResp struct {
					Error string `json:"error"`
				}
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&errResp))

				assert.Contains(t, errResp.Error, "NotAPhase",
					"error message should mention the invalid value")
			},
		},
		{
			name:       "Empty phase returns 400",
			phase:      "",
			wantStatus: http.StatusBadRequest,
		},
	}

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

	// ── Run test cases ──────────────────────────────────────────────────────

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			patchBody := mustMarshal(t, map[string]string{"phase": tc.phase})
			resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)

			require.Equal(t, tc.wantStatus, resp.StatusCode,
				"phase %q: expected %d", tc.phase, tc.wantStatus)

			if tc.validate != nil {
				tc.validate(t, resp)
			} else {
				drainBody(resp)
			}
		})
	}

	// ── Cleanup ─────────────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName, nil)
	assert.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, resp.StatusCode, "delete project")
	drainBody(resp)
}
