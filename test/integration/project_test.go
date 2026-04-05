//go:build e2e

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
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

// TestProjectNameValidation verifies that project creation rejects names that
// violate DNS subdomain naming rules and accepts valid names.
func TestProjectNameValidation(t *testing.T) {
	type testCase struct {
		name        string
		projectName string
		wantStatus  int
	}

	validService := v1.ProjectSpec{
		Services: []v1.ServiceDef{
			{Name: "web", Image: "nginx:latest"},
		},
	}

	testCases := []testCase{
		{
			name:        "Valid DNS-style name returns 201",
			projectName: "e2e-name-val-project-01",
			wantStatus:  http.StatusCreated,
		},
		{
			name:        "Single char name returns 201",
			projectName: "b",
			wantStatus:  http.StatusCreated,
		},
		{
			name:        "Name with spaces returns 400",
			projectName: "project with spaces",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "Name with special characters returns 400",
			projectName: "project&special!chars",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "1000-character name returns 400",
			projectName: strings.Repeat("x", 1000),
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "Uppercase name returns 400",
			projectName: "MyProject",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "Name starting with dot returns 400",
			projectName: ".invalid-start",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "Name ending with dot returns 400",
			projectName: "invalid-end.",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "Name with dots is valid returns 201",
			projectName: "project.example.com",
			wantStatus:  http.StatusCreated,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body := mustMarshal(t, v1.Project{
				ObjectMeta: v1.ObjectMeta{Name: tc.projectName},
				Spec:       validService,
			})
			resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)

			require.Equal(t, tc.wantStatus, resp.StatusCode,
				"name %q: expected %d", tc.projectName, tc.wantStatus)
			drainBody(resp)
		})
	}

	// Cleanup: delete successfully created projects.
	for _, name := range []string{"e2e-name-val-project-01", "b", "project.example.com"} {
		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
		assert.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, resp.StatusCode,
			"cleanup delete %q", name)
		drainBody(resp)
	}
}
