//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createFailedProject creates a Project reported Failed with reason on node,
// failedFor ago, that has already been moved off failedNodes.
func createFailedProject(t *testing.T, s *controllerhelper.Suite, name, reason, node string, failedFor time.Duration, failedNodes ...string) {
	t.Helper()
	require.NoError(t, s.Store.CreateProject(context.Background(), &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeSmall},
		Status: v1.ProjectStatus{
			Phase:       v1.ProjectPhaseFailed,
			NodeRef:     node,
			FailedNodes: failedNodes,
			Conditions: []v1.Condition{{
				Type:               v1.ConditionTypePhase,
				Status:             v1.ConditionTrue,
				Reason:             reason,
				LastTransitionTime: time.Now().UTC().Add(-failedFor),
			}},
		},
	}))
}

// TestFailedProjectMovesToAnotherNode validates Tier-2 recovery end to end: a
// project reported Failed is returned to Pending, and the scheduler places it
// on a node other than the one it failed on, however it would otherwise rank.
func TestFailedProjectMovesToAnotherNode(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	// node-a sorts first and is the primary tier, so without the exclusion the
	// scheduler would put the project straight back on it.
	createLedgerNode(t, s, "move-node-a", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "move-node-b", "backup", controllerhelper.RoomyAllocatable(), false)
	createFailedProject(t, s, "move-project", "LocalRestartExhausted", "move-node-a", 2*time.Minute)
	s.Start(t)

	ctx := context.Background()
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "project rescheduled onto move-node-b", func() bool {
		p, err := s.Store.GetProject(ctx, "move-project")
		return err == nil && p.Status.Phase == v1.ProjectPhaseScheduled && p.Status.NodeRef == "move-node-b"
	})

	p, err := s.Store.GetProject(ctx, "move-project")
	require.NoError(t, err)
	assert.Equal(t, []string{"move-node-a"}, p.Status.FailedNodes, "the node it left is recorded")
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
