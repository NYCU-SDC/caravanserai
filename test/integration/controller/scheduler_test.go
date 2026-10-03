//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	ctrl "NYCU-SDC/caravanserai/internal/server/controller"
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

// TestNodeReadyAdapterListReadyNodes validates that the scheduler's node
// listing keeps only Ready, schedulable Nodes and carries their labels and
// Allocatable through from PostgreSQL.
func TestNodeReadyAdapterListReadyNodes(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	ctx := context.Background()
	allocatable := v1.ResourceList{"cpu": "3500m", "memory": "7680Mi"}

	nodes := []*v1.Node{
		{
			ObjectMeta: v1.ObjectMeta{Name: "ready-primary", Labels: map[string]string{"cara.io/tier": "primary", "team": "sdc"}},
			Status:     v1.NodeStatus{State: v1.NodeStateReady, Allocatable: allocatable},
		},
		{
			ObjectMeta: v1.ObjectMeta{Name: "ready-unlabeled"},
			Status:     v1.NodeStatus{State: v1.NodeStateReady},
		},
		{
			ObjectMeta: v1.ObjectMeta{Name: "not-ready", Labels: map[string]string{"cara.io/tier": "primary"}},
			Status:     v1.NodeStatus{State: v1.NodeStateNotReady, Allocatable: allocatable},
		},
		{
			ObjectMeta: v1.ObjectMeta{Name: "cordoned", Labels: map[string]string{"cara.io/tier": "backup"}},
			Spec:       v1.NodeSpec{Unschedulable: true},
			Status:     v1.NodeStatus{State: v1.NodeStateReady, Allocatable: allocatable},
		},
	}
	for _, n := range nodes {
		n.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"}
		require.NoError(t, s.Store.CreateNode(ctx, n))
	}

	got, err := adapter.NewNodeReadyAdapter(s.Store).ListReadyNodes(ctx)
	require.NoError(t, err)

	byName := map[string]ctrl.ReadyNode{}
	for _, n := range got {
		byName[n.Name] = n
	}
	assert.ElementsMatch(t, []string{"ready-primary", "ready-unlabeled"}, keys(byName),
		"NotReady and Unschedulable nodes must be filtered out")

	assert.Equal(t, map[string]string{"cara.io/tier": "primary", "team": "sdc"}, byName["ready-primary"].Labels)
	assert.Equal(t, allocatable, byName["ready-primary"].Allocatable)
	assert.Empty(t, byName["ready-unlabeled"].Labels)
	assert.Empty(t, byName["ready-unlabeled"].Allocatable)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
