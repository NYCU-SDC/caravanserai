//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
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

// TestNodeFailureTriggersRescheduling validates:
//
//  1. Create 2 Nodes (both Ready) + 1 Project
//  2. Wait for project to be Scheduled to one node
//  3. Node heartbeat goes stale → NodeHealth marks it NotReady
//  4. Rescheduler resets project to Pending
//  5. Scheduler re-assigns project to the other (still Ready) node
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

	// Wait for project to be reset to Pending by the rescheduler.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "resch-project-01", v1.ProjectPhasePending)

	// Wait for project to be re-scheduled to the other node.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "resch-project-01", v1.ProjectPhaseScheduled)

	// Verify it was assigned to the other node.
	p, err = s.Store.GetProject(ctx, "resch-project-01")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase)
	assert.Equal(t, otherNode, p.Status.NodeRef)
}
