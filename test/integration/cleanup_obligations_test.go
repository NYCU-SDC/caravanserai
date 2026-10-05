//go:build e2e

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listCleanupTasks(t *testing.T, node string) []v1.CleanupTask {
	t.Helper()
	resp := doRequest(t, http.MethodGet, "/api/v1/nodes/"+node+"/cleanup-obligations", nil)
	defer drainBody(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list v1.CleanupTaskList
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.NotNil(t, list.Items, "an empty listing must be [], not null")
	return list.Items
}

// tasksFor keeps the tasks about one Project, so the assertions hold however
// many other Projects earlier tests left behind.
func tasksFor(tasks []v1.CleanupTask, project string) []v1.CleanupTask {
	var mine []v1.CleanupTask
	for _, task := range tasks {
		if task.ProjectName == project {
			mine = append(mine, task)
		}
	}
	return mine
}

// CARA-84: each unresolved obligation is listed to the Agent of the Node that
// owes it, and disappears once that Agent reports ResourcesRemoved.
func TestCleanupObligationListing(t *testing.T) {
	const name = "e2e-cleanup-listing"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
	released, err := adapter.NewProjectStoreAdapter(suite.store).ReleaseAssignment(ctx, snapshotOf(getProject(t, name)))
	require.NoError(t, err)
	require.True(t, released)

	tasks := tasksFor(listCleanupTasks(t, "node-a"), name)
	require.Len(t, tasks, 1)
	assert.Equal(t, v1.CleanupTask{
		Namespace:            v1.DefaultNamespace,
		ProjectName:          name,
		Assignment:           v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen},
		ReleaseAuthorization: v1.ReleaseByWorkloadStopped,
	}, tasks[0])
	assert.Empty(t, tasksFor(listCleanupTasks(t, "node-b"), name), "another node was handed node-a's cleanup")

	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "resources-removed", evidenceBody(uid, "node-a", gen)))
	assert.Empty(t, tasksFor(listCleanupTasks(t, "node-a"), name), "a resolved obligation is still listed")
}

// The Deletion obligation of a Terminating Project belongs to the Node that
// still holds the assignment and is tearing the Project down; it is resolved
// by that teardown, never handed out as a separate task.
func TestCleanupObligationListingSkipsCurrentAssignment(t *testing.T) {
	const name = "e2e-cleanup-terminating"
	createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)

	resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
	drainBody(resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, getProject(t, name).Status.Recovery.CleanupObligations, 1)

	assert.Empty(t, tasksFor(listCleanupTasks(t, "node-a"), name))
}

// A Node with nothing owed, including one that has no Node record at all, gets
// an empty list rather than an error: the obligation list is authoritative,
// not the Node record.
func TestCleanupObligationListingUnknownNode(t *testing.T) {
	assert.Empty(t, listCleanupTasks(t, "e2e-no-such-node"))
}
