//go:build e2e

package integration

import (
	"NYCU-SDC/caravanserai/test/integration/testhelper"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/authz"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Node authorization end to end (CARA-140): node-a and node-b each run a
// Project with its own Secret, and node-a still owes cleanup for a Project
// it released. The store schedules that Project again only after the cleanup
// is reported, so it waits in Pending meanwhile.
func TestNodeAuthorization(t *testing.T) {
	ctx := context.Background()
	sched := adapter.NewProjectStoreAdapter(suite.store)
	const (
		nodeA, nodeB         = "authz-node-a", "authz-node-b"
		projA, projB, moved  = "authz-proj-a", "authz-proj-b", "authz-moved"
		secretA, secretB     = "authz-secret-a", "authz-secret-b"
		secretMoved, unknown = "authz-secret-moved", "authz-secret-unused"
	)
	bearerA := "Bearer " + issueToken(t, v1.APITokenKindNode, nodeA)
	bearerB := "Bearer " + issueToken(t, v1.APITokenKindNode, nodeB)

	for _, s := range []string{secretA, secretB, secretMoved, unknown} {
		drainBody(doRequest(t, http.MethodDelete, "/api/v1/secrets/"+s, nil))
		resp := doRequest(t, http.MethodPost, "/api/v1/secrets", mustMarshal(t, v1.Secret{
			ObjectMeta: v1.ObjectMeta{Name: s},
			Spec:       v1.SecretSpec{Data: []v1.SecretDataItem{{Key: "k", Value: "v"}}},
		}))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		drainBody(resp)
	}
	create := func(name, secret, node string) v1.Project {
		drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
		resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec: v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine", Env: []v1.EnvVar{{
				Name: "X", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Name: secret, Key: "k"}},
			}}}}},
		}))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		drainBody(resp)
		require.NoError(t, sched.SetProjectScheduled(ctx, name, testhelper.UID(t, suite.store, name), node))
		t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil)) })
		return getProject(t, name)
	}
	a := create(projA, secretA, nodeA)
	b := create(projB, secretB, nodeB)

	// moved: node-a held it and released it without reporting cleanup yet.
	m := create(moved, secretMoved, nodeA)
	oldFence := store.AssignmentRef{UID: m.ObjectMeta.UID, NodeRef: nodeA, Generation: m.Status.AssignmentGeneration}
	require.NoError(t, suite.store.RecordWorkloadStopped(ctx, moved, oldFence, nil, time.Now()))
	released, err := sched.ReleaseAssignment(ctx, &controller.ProjectSnapshot{
		Name: moved, UID: oldFence.UID, Phase: m.Status.Phase, NodeRef: nodeA, AssignmentGeneration: oldFence.Generation,
	})
	require.NoError(t, err)
	require.True(t, released)
	require.Empty(t, getProject(t, moved).Status.NodeRef)

	status := func(bearer, method, path string, body []byte) int {
		t.Helper()
		resp := requestAs(t, bearer, method, path, body)
		drainBody(resp)
		return resp.StatusCode
	}
	fence := func(p v1.Project, node string, extra map[string]any) []byte {
		body := map[string]any{"uid": p.ObjectMeta.UID, "nodeRef": node, "assignmentGeneration": p.Status.AssignmentGeneration}
		for k, v := range extra {
			body[k] = v
		}
		return mustMarshal(t, body)
	}

	t.Run("a Node reads only the Secrets its own Projects reference", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, status(bearerA, http.MethodGet, "/api/v1/secrets/"+secretA, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodGet, "/api/v1/secrets/"+secretB, nil))
		assert.Equal(t, http.StatusOK, status(bearerB, http.MethodGet, "/api/v1/secrets/"+secretB, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodGet, "/api/v1/secrets/"+unknown, nil))
		// Cleanup needs no Secret.
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodGet, "/api/v1/secrets/"+secretMoved, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodGet, "/api/v1/secrets", nil), "listing Secrets")
	})

	t.Run("a Node cannot report with another Node's fence", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPatch, "/api/v1/projects/"+projB+"/status",
			fence(b, nodeB, map[string]any{"phase": "Running"})))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPut, "/api/v1/projects/"+projB+"/recovery/workload-stopped",
			fence(b, nodeB, nil)))
		// Its own name on someone else's assignment is the store's to refuse.
		assert.Equal(t, http.StatusConflict, status(bearerA, http.MethodPatch, "/api/v1/projects/"+projB+"/status",
			fence(b, nodeA, map[string]any{"phase": "Running"})))
		assert.Equal(t, v1.ProjectPhaseScheduled, getProject(t, projB).Status.Phase, "node-b's Project unchanged")
		// And its own report still works.
		assert.Equal(t, http.StatusNoContent, status(bearerA, http.MethodPatch, "/api/v1/projects/"+projA+"/status",
			fence(a, nodeA, map[string]any{"phase": "Running"})))
		// Restarting a Project is a user's decision, even its own Project's.
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPost, "/api/v1/projects/"+projA+"/restart", nil))
	})

	t.Run("a Node sees only its own Projects", func(t *testing.T) {
		resp := requestAs(t, bearerA, http.MethodGet, "/api/v1/projects?nodeRef="+nodeB, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var list v1.ProjectList
		mustDecodeBody(t, resp, &list)
		var names []string
		for _, p := range list.Items {
			names = append(names, p.Name)
		}
		assert.Equal(t, []string{projA}, names)

		assert.Equal(t, http.StatusOK, status(bearerA, http.MethodGet, "/api/v1/projects/"+projA, nil))
		assert.Equal(t, http.StatusNotFound, status(bearerA, http.MethodGet, "/api/v1/projects/"+projB, nil))
	})

	t.Run("the old Node finishes cleanup for a Project it released", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, status(bearerA, http.MethodGet, "/api/v1/projects/"+moved, nil))
		resp := requestAs(t, bearerA, http.MethodGet, "/api/v1/nodes/"+nodeA+"/cleanup-obligations", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var tasks v1.CleanupTaskList
		mustDecodeBody(t, resp, &tasks)
		require.Len(t, tasks.Items, 1)
		assert.Equal(t, moved, tasks.Items[0].ProjectName)

		assert.Equal(t, http.StatusNoContent, status(bearerA, http.MethodPut, "/api/v1/projects/"+moved+"/recovery/resources-removed",
			mustMarshal(t, map[string]any{"uid": oldFence.UID, "nodeRef": nodeA, "assignmentGeneration": oldFence.Generation})))
		// Nothing left to do there: node-a has no business with it any more.
		assert.Equal(t, http.StatusNotFound, status(bearerA, http.MethodGet, "/api/v1/projects/"+moved, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodGet, "/api/v1/nodes/"+nodeB+"/cleanup-obligations", nil))
	})

	t.Run("a Node cannot create, update or delete Projects", func(t *testing.T) {
		body := mustMarshal(t, v1.Project{ObjectMeta: v1.ObjectMeta{Name: "authz-by-node"},
			Spec: v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}}})
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPost, "/api/v1/projects", body))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPut, "/api/v1/projects/"+projA, body))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodDelete, "/api/v1/projects/"+projA, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodDelete, "/api/v1/nodes/"+nodeB, nil))
		assert.Equal(t, http.StatusForbidden, status(bearerA, http.MethodPost, "/api/v1/nodes/"+nodeB+"/heartbeat", nil))
		assert.Equal(t, v1.ProjectPhaseRunning, getProject(t, projA).Status.Phase, "still there")
	})
}

// Every route on the Node allow-list is still registered: a route renamed in
// a handler but not in authz would be closed to Agents without any test
// noticing. Sent as the admin, which authz lets through, so the response
// comes from the route's handler (JSON) rather than the mux's plain-text
// 404/405.
func TestNodeAllowListRoutesAreRegistered(t *testing.T) {
	for _, pattern := range authz.NodeRoutes() {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{name}", "authz-route-check", "{type}", "Maintenance",
			"{backupID}", "20261106T000000Z-0a1b2c3d").Replace(path)
		resp := doRequest(t, method, path, []byte("{}"))
		drainBody(resp)
		assert.NotContains(t, resp.Header.Get("Content-Type"), "text/plain",
			"%s answered %d from the mux: the route is not registered", pattern, resp.StatusCode)
	}
}
