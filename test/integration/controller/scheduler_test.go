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
			Allocatable:   controllerhelper.RoomyAllocatable(),
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

// TestCapacityLedgerAgainstPostgres validates that the ledger books only the
// phases that hold resources, reads each Project's size back from PostgreSQL,
// and applies the fit check to the sum.
func TestCapacityLedgerAgainstPostgres(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	ctx := context.Background()
	const node = "ledger-node"

	create := func(name string, phase v1.ProjectPhase, nodeRef string, size v1.ProjectSize) {
		t.Helper()
		require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec:       v1.ProjectSpec{Size: size},
			Status:     v1.ProjectStatus{Phase: phase, NodeRef: nodeRef},
		}))
	}
	create("running-large", v1.ProjectPhaseRunning, node, v1.ProjectSizeLarge)         // 2c / 4Gi
	create("terminating-small", v1.ProjectPhaseTerminating, node, v1.ProjectSizeSmall) // 0.5c / 512Mi
	create("scheduled-default", v1.ProjectPhaseScheduled, node, "")                    // omitted → Medium: 1c / 2Gi
	create("failed-large", v1.ProjectPhaseFailed, node, v1.ProjectSizeLarge)           // not booked
	create("pending-large", v1.ProjectPhasePending, "", v1.ProjectSizeLarge)           // not on the node
	create("elsewhere-large", v1.ProjectPhaseRunning, "other-node", v1.ProjectSizeLarge)

	ledger := ctrl.NewCapacityLedger(shared.logger, adapter.NewProjectStoreAdapter(s.Store))

	used, err := ledger.Used(ctx, node)
	require.NoError(t, err)
	assert.Equal(t, ctrl.NodeUsage{CPUMilli: 3500, MemoryBytes: 4<<30 + 512<<20 + 2<<30}, used,
		"only Running, Terminating and Scheduled projects on the node are booked")

	// 3.5c / 6.5Gi booked. 4c / 8Gi allocatable leaves 0.5c / 1.5Gi.
	n := ctrl.ReadyNode{Name: node, Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}}

	ok, reason, err := ledger.Fits(ctx, n, v1.ProjectSizeSmall)
	require.NoError(t, err)
	assert.True(t, ok, "Small fits the remaining 0.5c / 1.5Gi exactly on cpu: %s", reason)

	ok, reason, err = ledger.Fits(ctx, n, v1.ProjectSizeMedium)
	require.NoError(t, err)
	assert.False(t, ok, "Medium needs 1c but only 0.5c remains")
	assert.Contains(t, reason, "lacks cpu")
}

// createLedgerNode creates a Ready node with the given Allocatable and, when
// busy is true, a Running Large project that fills 2c / 4Gi of it. tier, when
// not empty, is set as the node's cara.io/tier label.
func createLedgerNode(t *testing.T, s *controllerhelper.Suite, name, tier string, allocatable v1.ResourceList, busy bool) {
	t.Helper()
	ctx := context.Background()
	var labels map[string]string
	if tier != "" {
		labels = map[string]string{v1.LabelNodeTier: tier}
	}
	require.NoError(t, s.Store.CreateNode(ctx, &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: name, Labels: labels},
		Status:     v1.NodeStatus{State: v1.NodeStateReady, Allocatable: allocatable, LastHeartbeat: time.Now().UTC()},
	}))
	if busy {
		require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
			ObjectMeta: v1.ObjectMeta{Name: name + "-tenant"},
			Spec:       v1.ProjectSpec{Size: v1.ProjectSizeLarge},
			Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: name},
		}))
	}
}

// TestSchedulerFilterSkipsFullNode validates that the scheduler leaves out a
// node whose Allocatable is already booked and places the project on the one
// with room.
func TestSchedulerFilterSkipsFullNode(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "filter-full", "", v1.ResourceList{"cpu": "2", "memory": "4Gi"}, true)
	createLedgerNode(t, s, "filter-roomy", "", v1.ResourceList{"cpu": "8", "memory": "16Gi"}, false)
	s.Start(t)

	require.NoError(t, s.Store.CreateProject(context.Background(), &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "filter-project"},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeMedium},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhasePending},
	}))

	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "filter-project", v1.ProjectPhaseScheduled)
	p, err := s.Store.GetProject(context.Background(), "filter-project")
	require.NoError(t, err)
	assert.Equal(t, "filter-roomy", p.Status.NodeRef, "the full node must not be chosen")
}

// TestSchedulerFilterNoRoomStaysPending validates that a project that fits on
// no node is left Pending rather than over-committed.
func TestSchedulerFilterNoRoomStaysPending(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "filter-full", "", v1.ResourceList{"cpu": "2", "memory": "4Gi"}, true)
	createLedgerNode(t, s, "filter-unreported", "", nil, false) // an agent that predates CARA-111
	s.Start(t)

	ctx := context.Background()
	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "filter-project"},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeMedium},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhasePending},
	}))

	assert.Never(t, func() bool {
		p, err := s.Store.GetProject(ctx, "filter-project")
		return err != nil || p.Status.Phase != v1.ProjectPhasePending
	}, 3*time.Second, 200*time.Millisecond, "a project that fits nowhere must stay Pending")
}

// scheduleOnePending creates a Pending Medium project and returns the node the
// scheduler places it on.
func scheduleOnePending(t *testing.T, s *controllerhelper.Suite, name string) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeMedium},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhasePending},
	}))
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, name, v1.ProjectPhaseScheduled)
	p, err := s.Store.GetProject(ctx, name)
	require.NoError(t, err)
	return p.Status.NodeRef
}

// TestSchedulerScorePrefersPrimary validates that with room on both tiers the
// project goes to the primary node, even though the backup node is emptier
// and sorts first.
func TestSchedulerScorePrefersPrimary(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "score-a-backup", "backup", v1.ResourceList{"cpu": "16", "memory": "64Gi"}, false)
	createLedgerNode(t, s, "score-z-primary", "primary", v1.ResourceList{"cpu": "4", "memory": "8Gi"}, true)
	s.Start(t)

	assert.Equal(t, "score-z-primary", scheduleOnePending(t, s, "score-project"))
}

// TestSchedulerScoreFallsBackToBackup validates that when no primary node has
// room the project goes to a backup node.
func TestSchedulerScoreFallsBackToBackup(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	// 2c / 4Gi allocatable is filled by the Running Large project: no room.
	createLedgerNode(t, s, "score-primary-full", "primary", v1.ResourceList{"cpu": "2", "memory": "4Gi"}, true)
	createLedgerNode(t, s, "score-backup", "backup", v1.ResourceList{"cpu": "4", "memory": "8Gi"}, false)
	s.Start(t)

	assert.Equal(t, "score-backup", scheduleOnePending(t, s, "score-project"))
}
