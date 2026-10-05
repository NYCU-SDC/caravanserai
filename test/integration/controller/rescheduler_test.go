//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a controllable clock for testing time-sensitive controllers.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time                  { return c.now }
func (c *fakeClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

// TestNodeFailureTriggersRescheduling validates the release gate of ADR-0017
// end to end, through the real controllers:
//
//  1. Create 2 Nodes (both Ready) + 1 Project
//  2. Wait for project to be Scheduled to one node
//  3. Node heartbeat goes stale → NodeHealth marks it NotReady
//  4. Without WorkloadStopped the Rescheduler keeps the assignment and marks
//     the project TakeoverRequired
//  5. The Agent's WorkloadStopped lets the Rescheduler release it to Pending,
//     recording a cleanup obligation
//  6. The Scheduler does not reassign it while that obligation is unresolved
//  7. After ResourcesRemoved the Scheduler re-assigns it to the other node
func TestNodeFailureTriggersRescheduling(t *testing.T) {
	// Use a fake clock for NodeHealth so we can control heartbeat age.
	clock := &fakeClock{now: time.Now().UTC()}

	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger,
		controller.WithClock(clock),
	)
	s.TruncateAll(t)

	ctx := context.Background()

	// Create two Ready nodes with fresh heartbeats.
	for _, name := range []string{"resch-node-01", "resch-node-02"} {
		node := &v1.Node{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec:       v1.NodeSpec{Hostname: name},
			Status: v1.NodeStatus{
				State:         v1.NodeStateReady,
				LastHeartbeat: clock.Now(),
			},
		}
		require.NoError(t, s.Store.CreateNode(ctx, node))
	}

	// Create a Pending project.
	project := &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "resch-project-01"},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhasePending,
		},
	}
	require.NoError(t, s.Store.CreateProject(ctx, project))

	// Start the controller manager.
	s.Start(t)

	// Wait for project to be scheduled.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "resch-project-01", v1.ProjectPhaseScheduled)

	// Determine which node was assigned.
	p, err := s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assignedNode := p.Status.NodeRef
	require.NotEmpty(t, assignedNode)

	otherNode := "resch-node-01"
	if assignedNode == "resch-node-01" {
		otherNode = "resch-node-02"
	}

	// Simulate heartbeat timeout: advance clock beyond 90s for the assigned
	// node.  The assigned node's heartbeat was set to clock.Now() at creation
	// time, so advancing the clock by >90s makes it stale.  The other node
	// also has the same heartbeat, so we need to update its heartbeat to
	// keep it fresh.
	clock.now = clock.now.Add(2 * time.Minute)

	// Keep the other node healthy by updating its heartbeat.
	otherNodeObj, err := s.Store.GetNode(ctx, otherNode)
	require.NoError(t, err)
	otherNodeObj.Status.LastHeartbeat = clock.Now()
	require.NoError(t, s.Store.UpdateNodeStatus(ctx, otherNode, otherNodeObj.Status))

	// Wait for NodeHealth to mark the assigned node NotReady.
	controllerhelper.WaitForNodeState(t, s.Store, 15*time.Second, assignedNode, v1.NodeStateNotReady)

	// Without stop evidence the release is refused: the project stays on the
	// NotReady node, marked TakeoverRequired.
	controllerhelper.WaitForCondition(t, 15*time.Second, 50*time.Millisecond, "TakeoverRequired", func() bool {
		p, err := s.Store.GetProject(ctx, "resch-project-01")
		return err == nil && hasCondition(p, v1.ConditionTypeTakeoverRequired)
	})
	p, err = s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase, "an unconfirmed release moved the project")
	assert.Equal(t, assignedNode, p.Status.NodeRef, "an unconfirmed release cleared the assignment")
	owner := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: assignedNode, Generation: p.Status.AssignmentGeneration}

	// The Agent confirms the workload stopped; the next reconcile releases.
	require.NoError(t, s.Store.RecordWorkloadStopped(ctx, "resch-project-01", owner, time.Now()))
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "resch-project-01", v1.ProjectPhasePending)
	p, err = s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assert.Empty(t, p.Status.NodeRef)
	assert.False(t, hasCondition(p, v1.ConditionTypeTakeoverRequired), "release kept TakeoverRequired")
	require.NotNil(t, p.Status.Recovery.FindObligation(v1.AssignmentIdentity{
		ProjectUID: owner.UID, NodeName: owner.NodeRef, AssignmentGeneration: owner.Generation,
	}), "release recorded no cleanup obligation")

	// Stopped is not removed: the Scheduler must leave the project Pending
	// across several of its one-second resync rounds while the obligation is
	// unresolved.
	time.Sleep(3 * time.Second)
	p, err = s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhasePending, p.Status.Phase, "scheduled while the old node still owed cleanup")

	// Cleanup reported: the Scheduler re-assigns to the other node.
	require.NoError(t, s.Store.RecordResourcesRemoved(ctx, "resch-project-01", owner, time.Now()))
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "resch-project-01", v1.ProjectPhaseScheduled)

	p, err = s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase)
	assert.Equal(t, otherNode, p.Status.NodeRef)
	assert.Equal(t, owner.Generation+1, p.Status.AssignmentGeneration)
}

func hasCondition(p *v1.Project, t v1.ConditionType) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}
