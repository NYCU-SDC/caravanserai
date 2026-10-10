//go:build e2e

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failedGeneration is the assignment generation createFailedProject gives the
// Failed assignment.
const failedGeneration = 2

// createFailedProject creates a Project reported Failed with reason on node,
// failedFor ago, that has already been moved off failedNodes.
func createFailedProject(t *testing.T, s *controllerhelper.Suite, name, reason, node string, failedFor time.Duration, failedNodes ...string) {
	t.Helper()
	require.NoError(t, s.Store.CreateProject(context.Background(), &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeSmall},
		Status: v1.ProjectStatus{
			Phase:                v1.ProjectPhaseFailed,
			NodeRef:              node,
			AssignmentGeneration: failedGeneration,
			AssignmentHistory:    v1.AssignmentHistoryKnown,
			FailedNodes:          failedNodes,
			Conditions: []v1.Condition{{
				Type:               v1.ConditionTypePhase,
				Status:             v1.ConditionTrue,
				Reason:             reason,
				LastTransitionTime: time.Now().UTC().Add(-failedFor),
			}},
		},
	}))
}

// failedAssignment is the store's fence for the assignment createFailedProject
// made.
func failedAssignment(t *testing.T, s *controllerhelper.Suite, name, node string) store.AssignmentRef {
	t.Helper()
	p, err := s.Store.GetProject(context.Background(), name)
	require.NoError(t, err)
	return store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: node, Generation: failedGeneration}
}

// TestFailedProjectMovesToAnotherNode validates Tier-2 recovery end to end: a
// project reported Failed, whose Agent has confirmed the workload stopped, is
// released with a cleanup obligation, and once the old node has removed its
// resources the scheduler places it on a node other than the one it failed on,
// however it would otherwise rank.
func TestFailedProjectMovesToAnotherNode(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	// node-a sorts first and is the primary tier, so without the exclusion the
	// scheduler would put the project straight back on it.
	createLedgerNode(t, s, "move-node-a", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "move-node-b", "backup", controllerhelper.RoomyAllocatable(), false)
	createFailedProject(t, s, "move-project", "LocalRestartExhausted", "move-node-a", 2*time.Minute)
	ref := failedAssignment(t, s, "move-project", "move-node-a")
	require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "move-project", ref, time.Now()))
	s.Start(t)

	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "project released", func() bool {
		p, err := s.Store.GetProject(ctx, "move-project")
		return err == nil && p.Status.Phase == v1.ProjectPhasePending
	})

	// Stopped is not removed: until move-node-a has cleaned up, no node may be
	// given the project.
	assert.Never(t, func() bool {
		p, err := s.Store.GetProject(ctx, "move-project")
		return err != nil || p.Status.Phase != v1.ProjectPhasePending
	}, 2*time.Second, 200*time.Millisecond, "a project with unremoved resources must not be scheduled")

	require.NoError(t, s.Store.RecordResourcesRemoved(ctx, "move-project", ref, time.Now()))
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "project rescheduled onto move-node-b", func() bool {
		p, err := s.Store.GetProject(ctx, "move-project")
		return err == nil && p.Status.Phase == v1.ProjectPhaseScheduled && p.Status.NodeRef == "move-node-b"
	})

	p, err := s.Store.GetProject(ctx, "move-project")
	require.NoError(t, err)
	assert.Equal(t, []string{"move-node-a"}, p.Status.FailedNodes, "the node it left is recorded")
}

// TestFailedProjectWithoutStopEvidenceStays validates that Failed is not taken
// as stopped: with no confirmation from its node, the project stays where it
// is, is marked TakeoverRequired, and spends none of its moves.
func TestFailedProjectWithoutStopEvidenceStays(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	createLedgerNode(t, s, "unconf-node-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "unconf-node-b", "", controllerhelper.RoomyAllocatable(), false)
	createFailedProject(t, s, "unconf-project", "LocalRestartExhausted", "unconf-node-a", time.Hour)
	s.Start(t)

	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "TakeoverRequired", func() bool {
		p, err := s.Store.GetProject(ctx, "unconf-project")
		return err == nil && hasProjectCondition(p, v1.ConditionTypeTakeoverRequired)
	})

	p, err := s.Store.GetProject(ctx, "unconf-project")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseFailed, p.Status.Phase)
	assert.Equal(t, "unconf-node-a", p.Status.NodeRef)
	assert.Empty(t, p.Status.FailedNodes, "a move that did not happen must not be counted")
	assert.False(t, hasProjectCondition(p, v1.ConditionTypeRescheduleExhausted))
	assert.Empty(t, obligationsOf(p), "nothing was released, so nothing is owed")
}

func obligationsOf(p *v1.Project) []v1.CleanupObligation {
	if p.Status.Recovery == nil {
		return nil
	}
	return p.Status.Recovery.CleanupObligations
}

func hasProjectCondition(p *v1.Project, t v1.ConditionType) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}

// TestMoveFailedProjectGate exercises the release gate directly against the
// store, so each case is decided by the guarded write and not by timing.
func TestMoveFailedProjectGate(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(s.Store)

	snapshot := func(name string) *controller.ProjectSnapshot {
		t.Helper()
		p, err := projects.GetProjectSnapshot(ctx, name)
		require.NoError(t, err)
		return p
	}
	unmoved := func(name string) {
		t.Helper()
		p, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseFailed, p.Status.Phase)
		assert.Equal(t, "gate-node-a", p.Status.NodeRef)
		assert.Empty(t, p.Status.FailedNodes, "no move may be counted")
		assert.Empty(t, obligationsOf(p))
		assert.True(t, hasProjectCondition(p, v1.ConditionTypeTakeoverRequired))
	}

	t.Run("no evidence", func(t *testing.T) {
		createFailedProject(t, s, "gate-none", "LocalRestartExhausted", "gate-node-a", time.Hour)
		moved, err := projects.MoveFailedProject(ctx, snapshot("gate-none"), "move")
		require.NoError(t, err)
		assert.False(t, moved)
		unmoved("gate-none")
	})

	t.Run("evidence for an earlier generation", func(t *testing.T) {
		createFailedProject(t, s, "gate-old", "LocalRestartExhausted", "gate-node-a", time.Hour)
		ref := failedAssignment(t, s, "gate-old", "gate-node-a")
		require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "gate-old", func(st *v1.ProjectStatus) error {
			st.EnsureRecovery().WorkloadStopped = &v1.WorkloadStoppedEvidence{
				Assignment: v1.AssignmentIdentity{ProjectUID: ref.UID, NodeName: "gate-node-a", AssignmentGeneration: failedGeneration - 1},
				ObservedAt: time.Now().UTC(),
			}
			return nil
		}))
		moved, err := projects.MoveFailedProject(ctx, snapshot("gate-old"), "move")
		require.NoError(t, err)
		assert.False(t, moved)
		unmoved("gate-old")
	})

	t.Run("evidence for the current assignment", func(t *testing.T) {
		createFailedProject(t, s, "gate-ok", "LocalRestartExhausted", "gate-node-a", time.Hour, "gate-node-z")
		ref := failedAssignment(t, s, "gate-ok", "gate-node-a")
		// A refusal first, so the release is seen clearing its own marker.
		moved, err := projects.MoveFailedProject(ctx, snapshot("gate-ok"), "move")
		require.NoError(t, err)
		require.False(t, moved)
		require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "gate-ok", ref, time.Now()))

		judged := snapshot("gate-ok")
		moved, err = projects.MoveFailedProject(ctx, judged, "move 2 of 2")
		require.NoError(t, err)
		assert.True(t, moved)

		p, err := s.Store.GetProject(ctx, "gate-ok")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhasePending, p.Status.Phase)
		assert.Empty(t, p.Status.NodeRef)
		assert.Equal(t, int64(failedGeneration), p.Status.AssignmentGeneration, "the counter is not reset")
		assert.Equal(t, []string{"gate-node-z", "gate-node-a"}, p.Status.FailedNodes)
		require.Len(t, p.Status.Recovery.CleanupObligations, 1)
		o := p.Status.Recovery.CleanupObligations[0]
		assert.Equal(t, v1.AssignmentIdentity{ProjectUID: ref.UID, NodeName: "gate-node-a", AssignmentGeneration: failedGeneration}, o.Assignment)
		assert.Equal(t, v1.ReleaseByWorkloadStopped, o.ReleaseAuthorization)
		assert.Nil(t, o.ResourcesRemoved)
		assert.False(t, hasProjectCondition(p, v1.ConditionTypeTakeoverRequired))
		version := p.ResourceVersion

		// Resending the same decision moves nothing and counts nothing.
		moved, err = projects.MoveFailedProject(ctx, judged, "move 2 of 2")
		require.NoError(t, err)
		assert.False(t, moved)
		again, err := s.Store.GetProject(ctx, "gate-ok")
		require.NoError(t, err)
		assert.Equal(t, version, again.ResourceVersion, "a resent move must not write")
		assert.Equal(t, []string{"gate-node-z", "gate-node-a"}, again.Status.FailedNodes)
		assert.Len(t, again.Status.Recovery.CleanupObligations, 1)
	})

	t.Run("a newer assignment is not moved by an older decision", func(t *testing.T) {
		createFailedProject(t, s, "gate-newer", "LocalRestartExhausted", "gate-node-a", time.Hour)
		ref := failedAssignment(t, s, "gate-newer", "gate-node-a")
		require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "gate-newer", ref, time.Now()))
		judged := snapshot("gate-newer")
		// Since the decision was read, the project was assigned again to the
		// same node and failed there too. The stop it carries is still the one
		// for generation 2; releasing on it would release generation 3, whose
		// containers nobody has confirmed stopped.
		require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "gate-newer", func(st *v1.ProjectStatus) error {
			st.AssignmentGeneration = failedGeneration + 1
			return nil
		}))

		moved, err := projects.MoveFailedProject(ctx, judged, "move")
		require.NoError(t, err)
		assert.False(t, moved)
		p, err := s.Store.GetProject(ctx, "gate-newer")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseFailed, p.Status.Phase, "the decision was about generation 2, not 3")
		assert.Empty(t, p.Status.FailedNodes)
		assert.Empty(t, obligationsOf(p))
	})

	t.Run("a project deleted since the decision is not moved", func(t *testing.T) {
		createFailedProject(t, s, "gate-deleted", "LocalRestartExhausted", "gate-node-a", time.Hour)
		ref := failedAssignment(t, s, "gate-deleted", "gate-node-a")
		require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "gate-deleted", ref, time.Now()))
		judged := snapshot("gate-deleted")
		require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "gate-deleted", func(st *v1.ProjectStatus) error {
			st.Phase = v1.ProjectPhaseTerminating
			return nil
		}))

		moved, err := projects.MoveFailedProject(ctx, judged, "move")
		require.NoError(t, err)
		assert.False(t, moved)
		p, err := s.Store.GetProject(ctx, "gate-deleted")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseTerminating, p.Status.Phase, "a move must not undo a deletion")
		assert.Equal(t, "gate-node-a", p.Status.NodeRef)
	})

	t.Run("a same-name successor is not moved", func(t *testing.T) {
		createFailedProject(t, s, "gate-recreated", "LocalRestartExhausted", "gate-node-a", time.Hour)
		judged := snapshot("gate-recreated")
		require.NoError(t, s.Store.DeleteProject(ctx, "gate-recreated"))
		createFailedProject(t, s, "gate-recreated", "LocalRestartExhausted", "gate-node-a", time.Hour)
		ref := failedAssignment(t, s, "gate-recreated", "gate-node-a")
		require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "gate-recreated", ref, time.Now()))

		moved, err := projects.MoveFailedProject(ctx, judged, "move")
		assert.False(t, moved)
		assert.True(t, errors.Is(err, store.ErrStaleAssignment), "got %v", err)
		p, getErr := s.Store.GetProject(ctx, "gate-recreated")
		require.NoError(t, getErr)
		assert.Equal(t, v1.ProjectPhaseFailed, p.Status.Phase, "the successor's decision was not made")
	})
}

// TestFailedProjectNotMovedForServiceFaults validates that a Failed project
// whose reason a new node cannot fix is left where it is.
func TestFailedProjectNotMovedForServiceFaults(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "stay-node-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "stay-node-b", "", controllerhelper.RoomyAllocatable(), false)
	createFailedProject(t, s, "stay-project", "SecretNotFound", "stay-node-a", time.Hour)
	s.Start(t)

	ctx := context.Background()
	assert.Never(t, func() bool {
		p, err := s.Store.GetProject(ctx, "stay-project")
		return err != nil || p.Status.Phase != v1.ProjectPhaseFailed || p.Status.NodeRef != "stay-node-a"
	}, 3*time.Second, 200*time.Millisecond, "a project failing on a Secret problem must not be moved")
}

// TestFailedProjectRescheduleExhausted validates that a project that has used
// all its moves stays Failed on its node and is marked for an operator.
func TestFailedProjectRescheduleExhausted(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "exh-node-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "exh-node-b", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "exh-node-c", "", controllerhelper.RoomyAllocatable(), false)
	createFailedProject(t, s, "exh-project", "LocalRestartExhausted", "exh-node-c", time.Hour, "exh-node-a", "exh-node-b")
	s.Start(t)

	ctx := context.Background()
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "RescheduleExhausted condition", func() bool {
		p, err := s.Store.GetProject(ctx, "exh-project")
		if err != nil {
			return false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == v1.ConditionTypeRescheduleExhausted && c.Status == v1.ConditionTrue {
				return true
			}
		}
		return false
	})

	p, err := s.Store.GetProject(ctx, "exh-project")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseFailed, p.Status.Phase, "it is left Failed, not moved again")
	assert.Equal(t, "exh-node-c", p.Status.NodeRef)
	assert.Equal(t, []string{"exh-node-a", "exh-node-b"}, p.Status.FailedNodes)
}

// TestApplyingAgainResetsRetryBudget validates that updating a Project's spec
// clears its failed nodes and the exhausted marker, and nothing else.
func TestApplyingAgainResetsRetryBudget(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()

	createFailedProject(t, s, "reapply-project", "LocalRestartExhausted", "node-c", time.Hour, "node-a", "node-b")
	require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "reapply-project", func(st *v1.ProjectStatus) error {
		st.Conditions = append(st.Conditions, v1.Condition{
			Type: v1.ConditionTypeRescheduleExhausted, Status: v1.ConditionTrue, Reason: "RetriesExhausted",
		})
		return nil
	}))

	before, err := s.Store.GetProject(ctx, "reapply-project")
	require.NoError(t, err)
	require.Len(t, before.Status.Conditions, 2)

	updated := *before
	updated.Spec.Size = v1.ProjectSizeLarge
	require.NoError(t, s.Store.UpdateProjectSpec(ctx, &updated))

	after, err := s.Store.GetProject(ctx, "reapply-project")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectSizeLarge, after.Spec.Size, "the spec was updated")
	assert.Empty(t, after.Status.FailedNodes, "the nodes it failed on are no longer ruled out")
	require.Len(t, after.Status.Conditions, 1, "only the exhausted marker is dropped")
	assert.Equal(t, v1.ConditionTypePhase, after.Status.Conditions[0].Type)
	assert.Equal(t, "LocalRestartExhausted", after.Status.Conditions[0].Reason)
	assert.Equal(t, v1.ProjectPhaseFailed, after.Status.Phase)
	assert.Equal(t, "node-c", after.Status.NodeRef)
}
