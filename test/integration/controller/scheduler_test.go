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

// TestFullSchedulingLifecycle validates:
//
//	Create a Node (Ready) + Project (Pending) →
//	Scheduler controller picks it up via event →
//	Project reaches Scheduled with nodeRef set.
func TestFullSchedulingLifecycle(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	s.Start(t)

	ctx := context.Background()

	// Create a Ready node.
	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: "sched-node-01"},
		Spec:       v1.NodeSpec{Hostname: "sched-node-01"},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			LastHeartbeat: time.Now().UTC(),
		},
	}
	err := s.Store.CreateNode(ctx, node)
	require.NoError(t, err)

	// Create a Pending project.
	project := &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "sched-project-01"},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhasePending,
		},
	}
	err = s.Store.CreateProject(ctx, project)
	require.NoError(t, err)

	// Wait for the project to be scheduled.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "sched-project-01", v1.ProjectPhaseScheduled)

	// Verify nodeRef is set.
	p, err := s.Store.GetProject(ctx, "sched-project-01")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase)
	assert.Equal(t, "sched-node-01", p.Status.NodeRef)
}
