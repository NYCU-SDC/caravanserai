//go:build e2e

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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

// The Agent's own client against the real server: the listing it decodes and
// the evidence it sends are the wire format the server serves and accepts.
func TestAgentClientResolvesCleanupObligation(t *testing.T) {
	const name = "e2e-cleanup-agent-client"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	require.Equal(t, http.StatusNoContent,
		putEvidence(t, name, "workload-stopped", evidenceBody(uid, "node-a", gen)))
	released, err := adapter.NewProjectStoreAdapter(suite.store).ReleaseAssignment(ctx, snapshotOf(getProject(t, name)))
	require.NoError(t, err)
	require.True(t, released)

	client := agent.NewClient(zap.NewNop(), suite.serverURL, "node-a")
	client.SetToken(testAdminToken)
	tasks, err := client.ListCleanupTasks(ctx)
	require.NoError(t, err)
	tasks = tasksFor(tasks, name)
	require.Len(t, tasks, 1)

	require.NoError(t, client.ReportResourcesRemoved(ctx, name, tasks[0].Assignment))
	o := getProject(t, name).Status.Recovery.FindObligation(tasks[0].Assignment)
	require.NotNil(t, o)
	assert.True(t, o.Resolved())

	// Another node reporting the same assignment is refused, and a project
	// that does not exist is reported as such.
	other := agent.NewClient(zap.NewNop(), suite.serverURL, "node-b")
	other.SetToken(testAdminToken)
	stale := tasks[0].Assignment
	stale.NodeName = "node-b"
	assert.ErrorIs(t, other.ReportResourcesRemoved(ctx, name, stale), agent.ErrStaleAssignment)
	assert.ErrorIs(t, client.ReportResourcesRemoved(ctx, "e2e-no-such-project", tasks[0].Assignment), agent.ErrProjectNotFound)
}

// TeardownBlocked is the agent's to write while it tears a Terminating Project
// down: the server accepts it under the current assignment's fence, refuses it
// under a stale one, and the agent can clear it again.
func TestTeardownBlockedConditionIsAgentWritable(t *testing.T) {
	const name = "e2e-teardown-blocked"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	resp := doRequest(t, http.MethodDelete, "/api/v1/projects/"+name, nil)
	drainBody(resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	client := agent.NewClient(zap.NewNop(), suite.serverURL, "node-a")
	client.SetToken(testAdminToken)
	fence := agent.AssignmentFence{UID: uid, NodeRef: "node-a", Generation: gen}

	stale := fence
	stale.Generation--
	assert.ErrorIs(t, client.PatchProjectCondition(ctx, name, stale, v1.ConditionTypeTeardownBlocked,
		v1.ConditionTrue, "RemoveFailed", "daemon busy"), agent.ErrStaleAssignment)

	require.NoError(t, client.PatchProjectCondition(ctx, name, fence, v1.ConditionTypeTeardownBlocked,
		v1.ConditionTrue, "RemoveFailed", "daemon busy"))
	p := getProject(t, name)
	require.Equal(t, v1.ProjectPhaseTerminating, p.Status.Phase)
	require.True(t, hasProjectCondition(p, v1.ConditionTypeTeardownBlocked))

	require.NoError(t, client.ClearProjectCondition(ctx, name, fence, v1.ConditionTypeTeardownBlocked))
	assert.False(t, hasProjectCondition(getProject(t, name), v1.ConditionTypeTeardownBlocked))
}

// CARA-92: the Agent's report that a Failed Project stopped is recorded as
// the current assignment's WorkloadStopped evidence — which is what lets
// Tier-2 recovery release it — and refused under any other assignment.
func TestAgentClientReportsWorkloadStopped(t *testing.T) {
	const name = "e2e-agent-workload-stopped"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseFailed)

	client := agent.NewClient(zap.NewNop(), suite.serverURL, "node-a")
	client.SetToken(testAdminToken)
	// Asked while the Project is still Failed on node-a, so the generation
	// is the only thing that differs.
	assert.ErrorIs(t, client.ReportWorkloadStopped(ctx, name, agent.AssignmentFence{UID: uid, NodeRef: "node-a", Generation: gen - 1}),
		agent.ErrStaleAssignment, "an older generation is refused")
	require.NoError(t, client.ReportWorkloadStopped(ctx, name, agent.AssignmentFence{UID: uid, NodeRef: "node-a", Generation: gen}))

	assert.True(t, getProject(t, name).Status.Recovery.WorkloadStoppedFor(
		v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}),
		"recorded for exactly the current assignment")
	released, err := adapter.NewProjectStoreAdapter(suite.store).MoveFailedProject(ctx, snapshotOf(getProject(t, name)), "moved")
	require.NoError(t, err)
	assert.True(t, released, "with the evidence the Failed project can be moved")

	assert.ErrorIs(t, client.ReportWorkloadStopped(ctx, "e2e-no-such-project", agent.AssignmentFence{UID: uid, NodeRef: "node-a", Generation: gen}),
		agent.ErrProjectNotFound)
}

// Review A1: the agent's evidence for a planned move is refused once that move
// was called off, so the Project is not left latched while it is meant to run
// where it is.
func TestAgentClientReportsAStopForAMoveThatWasCalledOff(t *testing.T) {
	const name = "e2e-agent-stop-for-move"
	ctx := context.Background()
	uid, gen := createAssigned(t, name, "node-a", v1.ProjectPhaseRunning)
	a := adapter.NewProjectStoreAdapter(suite.store)
	require.NoError(t, a.RequestEviction(ctx, name, "node-a", "Drain", ""))
	require.NoError(t, a.StartEvictionBackup(ctx, name, "node-a"))
	since := getProject(t, name).Status.Eviction.StateSince

	// Called off before the agent's evidence arrives.
	snapshots, err := a.ListEvictions(ctx)
	require.NoError(t, err)
	for _, e := range snapshots {
		if e.Name == name {
			require.NoError(t, a.EndEviction(ctx, e, &controller.EvictionFailure{Reason: "AgentDidNotStart", Message: "too slow"}))
		}
	}

	client := agent.NewClient(zap.NewNop(), suite.serverURL, "node-a")
	client.SetToken(testAdminToken)
	fence := agent.AssignmentFence{UID: uid, NodeRef: "node-a", Generation: gen}
	assert.ErrorIs(t, client.ReportWorkloadStoppedForMove(ctx, name, fence, since), agent.ErrStaleAssignment)
	assert.False(t, getProject(t, name).Status.Recovery.WorkloadStoppedFor(
		v1.AssignmentIdentity{ProjectUID: uid, NodeName: "node-a", AssignmentGeneration: gen}), "no evidence left behind")
}
