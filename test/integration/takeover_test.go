//go:build e2e

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOperatorToken = "test-operator-token"

func postTakeover(t *testing.T, name, token string, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost,
		suite.serverURL+"/api/v1/projects/"+name+"/recovery/takeover", bytes.NewReader(raw))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := suite.client.Do(req)
	require.NoError(t, err)
	return resp
}

func takeoverBody(p v1.Project, requestID string) map[string]any {
	return map[string]any{
		"uid":                  p.ObjectMeta.UID,
		"nodeRef":              p.Status.NodeRef,
		"assignmentGeneration": p.Status.AssignmentGeneration,
		"requestID":            requestID,
	}
}

// createAssignedProject leaves a Running Project on node-a, marked
// TakeoverRequired when marked is true.
func createAssignedProject(t *testing.T, name string, marked bool) v1.Project {
	t.Helper()
	ctx := context.Background()
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)

	require.NoError(t, adapter.NewProjectStoreAdapter(suite.store).SetProjectScheduled(ctx, name, "node-a"))
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, name, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		if marked {
			s.Conditions = v1.UpsertCondition(s.Conditions, v1.Condition{
				Type: v1.ConditionTypeTakeoverRequired, Status: v1.ConditionTrue, Reason: "WorkloadStopUnconfirmed",
			}, true)
		}
		return nil
	}))
	return getProject(t, name)
}

// TestOperatorTakeoverRequiresToken: without the credential nothing changes.
func TestOperatorTakeoverRequiresToken(t *testing.T) {
	const name = "e2e-takeover-token"
	p := createAssignedProject(t, name, true)

	for _, token := range []string{"", "wrong"} {
		resp := postTakeover(t, name, token, takeoverBody(p, "token-"+token))
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		drainBody(resp)
	}
	assert.Equal(t, "node-a", getProject(t, name).Status.NodeRef)
	records, err := suite.store.ListTakeoverAudit(context.Background(), name)
	require.NoError(t, err)
	assert.Empty(t, records)
}

// TestOperatorTakeoverRefusedStates: a healthy Project and a stale fence are
// refused, and a refusal leaves no audit record claiming a decision.
func TestOperatorTakeoverRefusedStates(t *testing.T) {
	ctx := context.Background()

	const healthy = "e2e-takeover-unmarked"
	p := createAssignedProject(t, healthy, false)
	resp := postTakeover(t, healthy, testOperatorToken, takeoverBody(p, "unmarked-"+p.ObjectMeta.UID))
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "a Project not marked TakeoverRequired must stay put")
	drainBody(resp)
	assert.Equal(t, "node-a", getProject(t, healthy).Status.NodeRef)

	const stale = "e2e-takeover-stale"
	p = createAssignedProject(t, stale, true)
	body := takeoverBody(p, "stale-"+p.ObjectMeta.UID)
	body["assignmentGeneration"] = p.Status.AssignmentGeneration + 1
	resp = postTakeover(t, stale, testOperatorToken, body)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, "node-a", getProject(t, stale).Status.NodeRef)

	for _, name := range []string{healthy, stale} {
		records, err := suite.store.ListTakeoverAudit(ctx, name)
		require.NoError(t, err)
		assert.Empty(t, records, name)
	}
}

// TestOperatorTakeoverReleases walks the lost-Node path: one approval lets the
// Project move to another Node, the old Node keeps its cleanup debt, a retry
// changes nothing, and the record survives the Project.
func TestOperatorTakeoverReleases(t *testing.T) {
	const name = "e2e-takeover-release"
	ctx := context.Background()
	p := createAssignedProject(t, name, true)
	requestID := "release-" + p.ObjectMeta.UID
	assignment := v1.AssignmentIdentity{ProjectUID: p.ObjectMeta.UID, NodeName: "node-a", AssignmentGeneration: p.Status.AssignmentGeneration}

	resp := postTakeover(t, name, testOperatorToken, takeoverBody(p, requestID))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)

	got := getProject(t, name)
	assert.Equal(t, v1.ProjectPhasePending, got.Status.Phase)
	assert.Empty(t, got.Status.NodeRef)
	for _, c := range got.Status.Conditions {
		assert.NotEqual(t, v1.ConditionTypeTakeoverRequired, c.Type)
	}
	assert.Nil(t, got.Status.Recovery.WorkloadStopped, "an override must not be recorded as stop evidence")
	require.Len(t, got.Status.Recovery.CleanupObligations, 1)
	o := got.Status.Recovery.CleanupObligations[0]
	assert.Equal(t, v1.ReleaseByOperatorOverride, o.ReleaseAuthorization)
	assert.False(t, o.Resolved(), "an override must not be recorded as cleanup evidence")
	assert.Len(t, got.CleanupTasksFor("node-a"), 1, "the old Node must still be told to clean up")

	// Retrying the same decision is a no-op, even with the fence the client held.
	resp = postTakeover(t, name, testOperatorToken, takeoverBody(p, requestID))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var replay struct {
		Record   v1.TakeoverAuditRecord `json:"record"`
		Replayed bool                   `json:"replayed"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&replay))
	drainBody(resp)
	assert.True(t, replay.Replayed)
	assert.Equal(t, v1.TakeoverReleased, replay.Record.Outcome)
	assert.Equal(t, "None", replay.Record.Evidence)

	// A request ID cannot be reused for a different decision.
	other := createAssignedProject(t, name+"-other", true)
	resp = postTakeover(t, other.Name, testOperatorToken, takeoverBody(other, requestID))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	drainBody(resp)
	assert.Equal(t, "node-a", getProject(t, other.Name).Status.NodeRef)

	// Back to the Node that owes cleanup is refused; another Node is not.
	stores := adapter.NewProjectStoreAdapter(suite.store)
	placement, err := stores.GetProjectPlacement(ctx, name)
	require.NoError(t, err)
	assert.Contains(t, placement.ExcludedNodes, "node-a")
	require.ErrorIs(t, stores.SetProjectScheduled(ctx, name, "node-a"), store.ErrConflictState)
	require.NoError(t, stores.SetProjectScheduled(ctx, name, "node-b"))
	got = getProject(t, name)
	assert.Equal(t, p.Status.AssignmentGeneration+1, got.Status.AssignmentGeneration)

	// The old Node returning resolves its debt, and the new assignment stands.
	require.NoError(t, suite.store.RecordResourcesRemoved(ctx, name, store.AssignmentRef{
		UID: assignment.ProjectUID, NodeRef: assignment.NodeName, Generation: assignment.AssignmentGeneration,
	}, time.Now()))
	got = getProject(t, name)
	assert.Equal(t, "node-b", got.Status.NodeRef)
	assert.Empty(t, got.Status.Recovery.NodesOwingCleanup())

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	records, err := suite.store.ListTakeoverAudit(ctx, name)
	require.NoError(t, err)
	require.Len(t, records, 1, "the retry must not add a record, and deletion must not remove it")
	assert.Equal(t, assignment, records[0].Assignment)
	assert.Contains(t, records[0].Caller, "operator-token@")
}

// TestOperatorTakeoverUnblocksWorkloadStoppedRelease is the state found on real
// VMs: evidence released the Project, then it waits forever for a Node that is
// never coming back.
func TestOperatorTakeoverUnblocksWorkloadStoppedRelease(t *testing.T) {
	const name = "e2e-takeover-pending"
	ctx := context.Background()
	p := createAssignedProject(t, name, true)
	ref := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: "node-a", Generation: p.Status.AssignmentGeneration}
	stores := adapter.NewProjectStoreAdapter(suite.store)

	require.NoError(t, suite.store.RecordWorkloadStopped(ctx, name, ref, time.Now()))
	released, err := stores.ReleaseAssignment(ctx, &controller.ProjectSnapshot{
		Name: name, UID: ref.UID, Phase: p.Status.Phase, NodeRef: ref.NodeRef, AssignmentGeneration: ref.Generation,
	})
	require.NoError(t, err)
	require.True(t, released)
	require.ErrorIs(t, stores.SetProjectScheduled(ctx, name, "node-b"), store.ErrConflictState)

	resp := postTakeover(t, name, testOperatorToken, map[string]any{
		"uid": ref.UID, "nodeRef": ref.NodeRef, "assignmentGeneration": ref.Generation, "requestID": "pending-" + ref.UID,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Record v1.TakeoverAuditRecord `json:"record"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	drainBody(resp)
	assert.Equal(t, v1.TakeoverObligationOverridden, out.Record.Outcome)
	assert.Equal(t, string(v1.ReleaseByWorkloadStopped), out.Record.Evidence)

	got := getProject(t, name)
	require.Len(t, got.Status.Recovery.CleanupObligations, 1, "the override must not add a second obligation")
	assert.False(t, got.Status.Recovery.CleanupObligations[0].Resolved())
	require.NoError(t, stores.SetProjectScheduled(ctx, name, "node-b"))
}

// TestGraceExpiredRelease: with the ADR-0024 transitional release on, only a
// Project with no Managed volume leaves on the grace period alone.
func TestGraceExpiredRelease(t *testing.T) {
	ctx := context.Background()
	on := adapter.NewProjectStoreAdapter(suite.store).WithGraceExpiredRelease(true)
	release := func(s *adapter.ProjectStoreAdapter, p v1.Project) bool {
		t.Helper()
		released, err := s.ReleaseAssignment(ctx, &controller.ProjectSnapshot{
			Name: p.Name, UID: p.ObjectMeta.UID, Phase: p.Status.Phase, NodeRef: p.Status.NodeRef, AssignmentGeneration: p.Status.AssignmentGeneration,
		})
		require.NoError(t, err)
		return released
	}

	p := createAssignedProject(t, "e2e-grace-off", false)
	assert.False(t, release(adapter.NewProjectStoreAdapter(suite.store), p), "off by default")
	assert.Equal(t, "node-a", getProject(t, p.Name).Status.NodeRef)

	p = createAssignedProject(t, "e2e-grace-stateless", false)
	require.True(t, release(on, p))
	got := getProject(t, p.Name)
	assert.Equal(t, v1.ProjectPhasePending, got.Status.Phase)
	assert.Nil(t, got.Status.Recovery.WorkloadStopped, "the grace period is not evidence the workload stopped")
	require.Len(t, got.Status.Recovery.CleanupObligations, 1)
	o := got.Status.Recovery.CleanupObligations[0]
	assert.Equal(t, v1.ReleaseByGraceExpired, o.ReleaseAuthorization)
	assert.False(t, o.Resolved(), "the old Node still owes cleanup")
	require.ErrorIs(t, on.SetProjectScheduled(ctx, p.Name, "node-a"), store.ErrConflictState, "not back to the Node that owes cleanup")
	require.NoError(t, on.SetProjectScheduled(ctx, p.Name, "node-b"))

	const managed = "e2e-grace-managed"
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+managed+"?force=true", nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: managed},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine", VolumeMounts: []v1.VolumeMount{{Name: "data", MountPath: "/data"}}}},
			Volumes:  []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
		},
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	require.NoError(t, on.SetProjectScheduled(ctx, managed, "node-a"))
	require.NoError(t, suite.store.UpdateProjectStatusWithRetry(ctx, managed, func(s *v1.ProjectStatus) error {
		s.Phase = v1.ProjectPhaseRunning
		return nil
	}))
	assert.False(t, release(on, getProject(t, managed)), "no committed backup: the only copy may be on the lost Node")
	got = getProject(t, managed)
	assert.Equal(t, "node-a", got.Status.NodeRef)

	// With a committed backup it moves, and the old Node owes its local data
	// before it may hold the Project again.
	uid, gen := got.ObjectMeta.UID, got.Status.AssignmentGeneration
	require.Equal(t, http.StatusNoContent,
		putEvidence(t, managed, "committed-backup", committedBackupBody(uid, "node-a", gen, "20261031T000000Z-aaaaaaaa")))
	require.True(t, release(on, getProject(t, managed)))
	got = getProject(t, managed)
	require.Len(t, got.Status.Recovery.CleanupObligations, 1)
	o = got.Status.Recovery.CleanupObligations[0]
	assert.Equal(t, v1.ReleaseByGraceExpired, o.ReleaseAuthorization)
	assert.True(t, o.ManagedDataOwed)

	require.Equal(t, http.StatusNoContent, putEvidence(t, managed, "resources-removed", evidenceBody(uid, "node-a", gen)))
	require.ErrorIs(t, on.SetProjectScheduled(ctx, managed, "node-a"), store.ErrConflictState, "local data still owed")

	require.Equal(t, http.StatusNoContent, putEvidence(t, managed, "managed-data-quarantined", evidenceBody(uid, "node-a", gen)))
	first := getProject(t, managed).Status.Recovery.CleanupObligations[0].ManagedDataQuarantined
	require.NotNil(t, first)
	before := getProject(t, managed).ObjectMeta.ResourceVersion
	require.Equal(t, http.StatusNoContent, putEvidence(t, managed, "managed-data-quarantined", evidenceBody(uid, "node-a", gen)))
	assert.Equal(t, before, getProject(t, managed).ObjectMeta.ResourceVersion, "first evidence wins")
	assert.Equal(t, http.StatusConflict, putEvidence(t, managed, "managed-data-quarantined", evidenceBody(uid, "node-b", gen)), "not this Node's obligation")

	// Back on the same Node it is a new generation that restores, per ADR-0024.
	require.NoError(t, on.SetProjectScheduled(ctx, managed, "node-a"))
	assert.Equal(t, gen+1, getProject(t, managed).Status.AssignmentGeneration)
	assert.Equal(t, "20261031T000000Z-aaaaaaaa", getProject(t, managed).Status.Recovery.CommittedBackupID())
}

// TestSchedulingBlockedCondition: the reason is on the Project while it waits
// and gone once it is assigned.
func TestSchedulingBlockedCondition(t *testing.T) {
	const name = "e2e-scheduling-blocked"
	ctx := context.Background()
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+name+"?force=true", nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	stores := adapter.NewProjectStoreAdapter(suite.store)

	find := func() *v1.Condition {
		p := getProject(t, name)
		for i := range p.Status.Conditions {
			if p.Status.Conditions[i].Type == v1.ConditionTypeSchedulingBlocked {
				return &p.Status.Conditions[i]
			}
		}
		return nil
	}

	require.NoError(t, stores.SetSchedulingBlocked(ctx, name, "NoSuitableNode", "node-b lacks cpu"))
	c := find()
	require.NotNil(t, c)
	assert.Equal(t, "node-b lacks cpu", c.Message)
	version := getProject(t, name).ObjectMeta.ResourceVersion
	require.NoError(t, stores.SetSchedulingBlocked(ctx, name, "NoSuitableNode", "node-b lacks cpu"))
	assert.Equal(t, version, getProject(t, name).ObjectMeta.ResourceVersion, "a repeated reason must not write")

	require.NoError(t, stores.SetProjectScheduled(ctx, name, "node-a"))
	assert.Nil(t, find(), "the assignment ends the wait")
	require.NoError(t, stores.SetSchedulingBlocked(ctx, name, "NoSuitableNode", "late report"))
	assert.Nil(t, find(), "a Project no longer Pending is not marked")
}
