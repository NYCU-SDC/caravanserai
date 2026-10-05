package v1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func fullRecoveryStatus() *ProjectRecoveryStatus {
	attempt := time.Date(2026, 10, 3, 9, 0, 0, 123456789, time.UTC)
	healthy := time.Date(2026, 10, 3, 9, 5, 0, 0, time.UTC)
	return &ProjectRecoveryStatus{
		RescheduleAttempts:     2,
		LastAttemptAt:          &attempt,
		HealthySince:           &healthy,
		HealthySinceGeneration: 7,
		PreferredNode:          "node-b",
		WorkloadStopped: &WorkloadStoppedEvidence{
			Assignment: AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 6},
			ObservedAt: time.Date(2026, 10, 3, 8, 59, 0, 0, time.UTC),
		},
		CleanupObligations: []CleanupObligation{
			{
				Assignment:           AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 5},
				ReleasedAt:           time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC),
				ReleaseAuthorization: ReleaseByWorkloadStopped,
				ResourcesRemoved:     &ResourcesRemovedEvidence{ObservedAt: time.Date(2026, 10, 3, 8, 30, 0, 0, time.UTC)},
			},
			{
				Assignment:           AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 6},
				ReleasedAt:           time.Date(2026, 10, 3, 9, 1, 0, 0, time.UTC),
				ReleaseAuthorization: ReleaseByWorkloadStopped,
			},
		},
	}
}

// Every recovery field must survive the JSON (API, PostgreSQL JSONB) and YAML
// (CLI) round trips. Times are compared as instants: a value that went through
// a codec and one built in-process can denote the same moment yet differ in
// their internal representation.
func TestProjectRecoveryStatus_RoundTrip(t *testing.T) {
	original := ProjectStatus{Phase: ProjectPhaseRunning, NodeRef: "node-b", AssignmentGeneration: 7, Recovery: fullRecoveryStatus()}

	assertSame := func(t *testing.T, got ProjectStatus) {
		t.Helper()
		require.NotNil(t, got.Recovery)
		want, have := original.Recovery, got.Recovery
		assert.Equal(t, want.RescheduleAttempts, have.RescheduleAttempts)
		assert.True(t, want.LastAttemptAt.Equal(*have.LastAttemptAt))
		assert.True(t, want.HealthySince.Equal(*have.HealthySince))
		assert.Equal(t, want.HealthySinceGeneration, have.HealthySinceGeneration)
		assert.Equal(t, want.PreferredNode, have.PreferredNode)
		assert.Equal(t, want.WorkloadStopped.Assignment, have.WorkloadStopped.Assignment)
		assert.True(t, want.WorkloadStopped.ObservedAt.Equal(have.WorkloadStopped.ObservedAt))
		require.Len(t, have.CleanupObligations, len(want.CleanupObligations))
		for i, w := range want.CleanupObligations {
			h := have.CleanupObligations[i]
			assert.Equal(t, w.Assignment, h.Assignment)
			assert.True(t, w.ReleasedAt.Equal(h.ReleasedAt))
			assert.Equal(t, w.ReleaseAuthorization, h.ReleaseAuthorization)
			assert.Equal(t, w.Resolved(), h.Resolved())
			if w.Resolved() {
				assert.True(t, w.ResourcesRemoved.ObservedAt.Equal(h.ResourcesRemoved.ObservedAt))
			}
		}
	}

	t.Run("json", func(t *testing.T) {
		raw, err := json.Marshal(original)
		require.NoError(t, err)
		var decoded ProjectStatus
		require.NoError(t, json.Unmarshal(raw, &decoded))
		assertSame(t, decoded)
	})

	t.Run("yaml", func(t *testing.T) {
		raw, err := yaml.Marshal(original)
		require.NoError(t, err)
		var decoded ProjectStatus
		require.NoError(t, yaml.Unmarshal(raw, &decoded))
		assertSame(t, decoded)
	})
}

// A Project that never entered recovery carries no recovery key at all, so
// existing statuses serialize exactly as before.
func TestProjectRecoveryStatus_OmittedWhenNil(t *testing.T) {
	raw, err := json.Marshal(ProjectStatus{Phase: ProjectPhaseRunning})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "recovery")
}

// A copy that shares any pointer with its source lets an edit to the copy
// change the source too. In the status update path that turns every in-place
// edit into a silent no-op, so each pointer field is mutated through the copy
// and the source checked untouched.
func TestProjectRecoveryStatus_DeepCopyDoesNotAlias(t *testing.T) {
	src := fullRecoveryStatus()
	before, err := json.Marshal(src)
	require.NoError(t, err)

	cp := src.DeepCopy()
	*cp.LastAttemptAt = cp.LastAttemptAt.Add(time.Hour)
	*cp.HealthySince = cp.HealthySince.Add(time.Hour)
	cp.WorkloadStopped.Assignment.AssignmentGeneration = 99
	cp.WorkloadStopped.ObservedAt = time.Time{}
	cp.CleanupObligations[0].Assignment.NodeName = "node-z"
	cp.CleanupObligations[0].ResourcesRemoved.ObservedAt = time.Time{}
	cp.CleanupObligations[1].ResourcesRemoved = &ResourcesRemovedEvidence{}
	cp.CleanupObligations = append(cp.CleanupObligations, CleanupObligation{})

	after, err := json.Marshal(src)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "editing the copy changed the source")
}

func TestProjectRecoveryStatus_DeepCopyNil(t *testing.T) {
	var r *ProjectRecoveryStatus
	assert.Nil(t, r.DeepCopy())

	sparse := &ProjectRecoveryStatus{RescheduleAttempts: 1}
	cp := sparse.DeepCopy()
	require.NotNil(t, cp)
	assert.Nil(t, cp.LastAttemptAt)
	assert.Nil(t, cp.WorkloadStopped)
	assert.Nil(t, cp.CleanupObligations)
}

func TestFindObligation(t *testing.T) {
	r := fullRecoveryStatus()
	got := r.FindObligation(AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 6})
	require.NotNil(t, got)
	assert.False(t, got.Resolved())

	// The returned pointer is into the list, so resolving through it is seen.
	got.ResourcesRemoved = &ResourcesRemovedEvidence{}
	assert.True(t, r.CleanupObligations[1].Resolved())

	assert.Nil(t, r.FindObligation(AssignmentIdentity{ProjectUID: "uid-2", NodeName: "node-a", AssignmentGeneration: 6}))
	var none *ProjectRecoveryStatus
	assert.Nil(t, none.FindObligation(AssignmentIdentity{}))
}

// An obligation records the first release of an assignment; releasing the same
// assignment again must not reset its time, authorization or evidence.
func TestAddObligationKeepsTheFirstEntry(t *testing.T) {
	r := fullRecoveryStatus()
	first := r.CleanupObligations[0]

	r.AddObligation(first.Assignment, first.ReleasedAt.Add(time.Hour), ReleaseByDeletion)
	require.Len(t, r.CleanupObligations, 2)
	assert.Equal(t, first, r.CleanupObligations[0])

	added := AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-b", AssignmentGeneration: 7}
	r.AddObligation(added, first.ReleasedAt, ReleaseByDeletion)
	require.Len(t, r.CleanupObligations, 3)
	assert.Equal(t, CleanupObligation{Assignment: added, ReleasedAt: first.ReleasedAt, ReleaseAuthorization: ReleaseByDeletion},
		r.CleanupObligations[2])
}

// Only cleanup, provider fencing or an operator override lets a Project be
// assigned past an obligation. Stopped workload and deletion are teardown
// facts, never evidence that the old Node can no longer run the workload.
//
// LeaseExpired is deliberately not listed: no lease exists yet, and its policy
// is decided with the lease, not pinned here.
func TestObligationPermitsAssignment(t *testing.T) {
	permits := map[ReleaseAuthorizationKind]bool{
		ReleaseByWorkloadStopped:  false,
		ReleaseByDeletion:         false,
		ReleaseByProviderFencing:  true,
		ReleaseByOperatorOverride: true,
	}
	for kind, want := range permits {
		o := CleanupObligation{ReleaseAuthorization: kind}
		assert.Equal(t, want, o.PermitsAssignment(), "unresolved %s", kind)
		o.ResourcesRemoved = &ResourcesRemovedEvidence{}
		assert.True(t, o.PermitsAssignment(), "resolved %s", kind)
	}
}

func TestBlockingObligation(t *testing.T) {
	var none *ProjectRecoveryStatus
	assert.Nil(t, none.BlockingObligation())

	r := fullRecoveryStatus()
	got := r.BlockingObligation()
	require.NotNil(t, got, "the unresolved WorkloadStopped obligation must block")
	assert.Equal(t, int64(6), got.Assignment.AssignmentGeneration)

	got.ResourcesRemoved = &ResourcesRemovedEvidence{}
	assert.Nil(t, r.BlockingObligation())
}

func TestWorkloadStoppedFor(t *testing.T) {
	r := fullRecoveryStatus()
	current := r.WorkloadStopped.Assignment
	assert.True(t, r.WorkloadStoppedFor(current))

	for _, other := range []AssignmentIdentity{
		{ProjectUID: current.ProjectUID, NodeName: current.NodeName, AssignmentGeneration: current.AssignmentGeneration + 1},
		{ProjectUID: current.ProjectUID, NodeName: "node-b", AssignmentGeneration: current.AssignmentGeneration},
		{ProjectUID: "uid-2", NodeName: current.NodeName, AssignmentGeneration: current.AssignmentGeneration},
	} {
		assert.False(t, r.WorkloadStoppedFor(other), "%+v", other)
	}

	var none *ProjectRecoveryStatus
	assert.False(t, none.WorkloadStoppedFor(current))
	assert.False(t, (&ProjectRecoveryStatus{}).WorkloadStoppedFor(current))
}
