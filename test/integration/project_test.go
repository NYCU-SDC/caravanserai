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
				var p problemResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))

				assert.Equal(t, http.StatusBadRequest, p.Status, "problem status must be 400")
				assert.Contains(t, p.Detail, "ClearlyBogusPhase",
					"detail should mention the invalid value")
				assert.Contains(t, p.Detail, "Pending",
					"detail should list valid phases")
			},
		},
		{
			name:       "Another invalid phase returns 400",
			phase:      "NotAPhase",
			wantStatus: http.StatusBadRequest,
			validate: func(t *testing.T, resp *http.Response) {
				var p problemResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))

				assert.Equal(t, http.StatusBadRequest, p.Status, "problem status must be 400")
				assert.Contains(t, p.Detail, "NotAPhase",
					"detail should mention the invalid value")
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

// TestProjectUpdate exercises PUT /api/v1/projects/{name}:
//
//	PUT on Pending project  → 200, spec updated
//	PUT on non-existent     → 404
//	PUT with name mismatch  → 400
//	PUT on Running project  → 409 (conflict state)
func TestProjectUpdate(t *testing.T) {
	const projectName = "e2e-project-update"

	validSpec := v1.ProjectSpec{
		Services: []v1.ServiceDef{
			{Name: "web", Image: "nginx:latest"},
		},
	}

	// ── Setup: create a project (starts in Pending) ───────────────────────────

	createBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec:       validSpec,
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create project: expected 201")
	drainBody(resp)

	// ── 1. PUT updates spec in Pending phase → 200 ────────────────────────────

	updatedSpec := v1.ProjectSpec{
		Services: []v1.ServiceDef{
			{Name: "api", Image: "myapp:v2"},
		},
	}
	updateBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: projectName},
		Spec:       updatedSpec,
	})
	resp = doRequest(t, http.MethodPut, "/api/v1/projects/"+projectName, updateBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "update project: expected 200")

	var updated v1.Project
	mustDecodeBody(t, resp, &updated)
	assert.Equal(t, projectName, updated.Name)
	require.Len(t, updated.Spec.Services, 1)
	assert.Equal(t, "api", updated.Spec.Services[0].Name, "service name should be updated")
	assert.Equal(t, "myapp:v2", updated.Spec.Services[0].Image, "service image should be updated")
	assert.Equal(t, v1.ProjectPhasePending, updated.Status.Phase, "phase should still be Pending")

	// ── 2. PUT on non-existent project → 404 ──────────────────────────────────

	nonExistentBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "no-such-project"},
		Spec:       validSpec,
	})
	resp = doRequest(t, http.MethodPut, "/api/v1/projects/no-such-project", nonExistentBody)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "update non-existent: expected 404")
	drainBody(resp)

	// ── 3. PUT with name mismatch → 400 ───────────────────────────────────────

	mismatchBody := mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "different-name"},
		Spec:       validSpec,
	})
	resp = doRequest(t, http.MethodPut, "/api/v1/projects/"+projectName, mismatchBody)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "name mismatch: expected 400")
	drainBody(resp)

	// ── 4. Transition to Running, then PUT → 409 ──────────────────────────────

	// Patch phase to Running via status endpoint.
	patchBody := mustMarshal(t, map[string]string{"phase": string(v1.ProjectPhaseRunning)})
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "patch to Running: expected 204")
	drainBody(resp)

	resp = doRequest(t, http.MethodPut, "/api/v1/projects/"+projectName, updateBody)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "update Running project: expected 409")
	drainBody(resp)

	// ── 5. Transition to Failed, then PUT → 200 (allowed) ────────────────────

	patchBody = mustMarshal(t, map[string]string{"phase": string(v1.ProjectPhaseFailed)})
	resp = doRequest(t, http.MethodPatch, "/api/v1/projects/"+projectName+"/status", patchBody)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "patch to Failed: expected 204")
	drainBody(resp)

	resp = doRequest(t, http.MethodPut, "/api/v1/projects/"+projectName, updateBody)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "update Failed project: expected 200")
	drainBody(resp)

	// ── Cleanup ───────────────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodDelete, "/api/v1/projects/"+projectName, nil)
	assert.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, resp.StatusCode, "delete project")
	drainBody(resp)
}

// TestProjectForceDelete verifies the ?force=true query parameter on
// DELETE /api/v1/projects/{name}:
//
//	Force-delete a Running project       → 204, project gone
//	Force-delete a Terminating project    → 204, project gone
//	Force-delete a Pending project        → 204 (same as normal delete)
//	Normal delete on Running project      → 202 (existing behaviour preserved)
func TestProjectForceDelete(t *testing.T) {
	// createProject is a helper that creates a project in Pending phase.
	createProject := func(t *testing.T, name string) {
		t.Helper()
		body := mustMarshal(t, v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec: v1.ProjectSpec{
				Services: []v1.ServiceDef{
					{Name: "web", Image: "nginx:latest"},
				},
			},
		})
		resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "create project %q", name)
		drainBody(resp)
	}

	// patchPhase transitions a project to the given phase via the status endpoint.
	patchPhase := func(t *testing.T, name string, phase v1.ProjectPhase) {
		t.Helper()
		body := mustMarshal(t, map[string]string{"phase": string(phase)})
		resp := doRequest(t, http.MethodPatch, "/api/v1/projects/"+name+"/status", body)
		require.Equal(t, http.StatusNoContent, resp.StatusCode, "patch %q to %s", name, phase)
		drainBody(resp)
	}

	// assertGone verifies a project no longer exists in the store.
	assertGone := func(t *testing.T, name string) {
		t.Helper()
		resp := doRequest(t, http.MethodGet, "/api/v1/projects/"+name, nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "project %q should be gone", name)
		drainBody(resp)
	}

	t.Run("Force-delete Running project returns 204 and removes from store", func(t *testing.T) {
		const name = "e2e-force-del-running"
		createProject(t, name)
		patchPhase(t, name, v1.ProjectPhaseRunning)

		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		drainBody(resp)

		assertGone(t, name)
	})

	t.Run("Force-delete Terminating project returns 204 and removes from store", func(t *testing.T) {
		const name = "e2e-force-del-terminating"
		createProject(t, name)
		patchPhase(t, name, v1.ProjectPhaseTerminating)

		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		drainBody(resp)

		assertGone(t, name)
	})

	t.Run("Force-delete Pending project returns 204", func(t *testing.T) {
		const name = "e2e-force-del-pending"
		createProject(t, name)

		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		drainBody(resp)

		assertGone(t, name)
	})

	t.Run("Normal delete on Running project returns 202 (existing behaviour)", func(t *testing.T) {
		const name = "e2e-normal-del-running"
		createProject(t, name)
		patchPhase(t, name, v1.ProjectPhaseRunning)

		resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		drainBody(resp)

		// Cleanup: force-delete so the project doesn't linger.
		resp = doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)
		assert.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, resp.StatusCode, "cleanup")
		drainBody(resp)
	})
}
