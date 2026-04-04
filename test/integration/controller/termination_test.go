//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/require"
)

// TestTerminationLifecycle validates:
//
//  1. Create a Node + Project, wait for Scheduled
//  2. Update project phase to Running (simulate Agent report)
//  3. Update project phase to Terminating (simulate delete request)
//  4. Update project phase to Terminated (simulate Agent cleanup)
//  5. Wait for TerminationController to delete the record from the store
func TestTerminationLifecycle(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	s.Start(t)

	ctx := context.Background()

	// Create a Ready node.
	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: "term-node-01"},
		Spec:       v1.NodeSpec{Hostname: "term-node-01"},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			LastHeartbeat: time.Now().UTC(),
		},
	}
	require.NoError(t, s.Store.CreateNode(ctx, node))

	// Create a Pending project.
	project := &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "term-project-01"},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhasePending,
		},
	}
	require.NoError(t, s.Store.CreateProject(ctx, project))

	// Wait for scheduling.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "term-project-01", v1.ProjectPhaseScheduled)

	// Simulate Agent reporting Running.
	p, err := s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseRunning
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Simulate delete request: set to Terminating.
	p, err = s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseTerminating
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Simulate Agent cleanup: set to Terminated.
	p, err = s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseTerminated
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Wait for TerminationController to delete the project from the store.
	controllerhelper.WaitForProjectNotFound(t, s.Store, 15*time.Second, "term-project-01")
}
