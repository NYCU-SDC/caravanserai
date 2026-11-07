//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setNodeScheduling sets a node's cordon and drain flags, as the CLI does.
func setNodeScheduling(t *testing.T, s *controllerhelper.Suite, name string, unschedulable, drain bool) {
	t.Helper()
	ctx := context.Background()
	n, err := s.Store.GetNode(ctx, name)
	require.NoError(t, err)
	n.Spec.Unschedulable, n.Spec.Drain = unschedulable, drain
	require.NoError(t, s.Store.UpdateNodeSpec(ctx, n))
}

func drainPhase(t *testing.T, s *controllerhelper.Suite, name string) v1.NodeDrainPhase {
	t.Helper()
	n, err := s.Store.GetNode(context.Background(), name)
	require.NoError(t, err)
	if n.Status.Drain == nil {
		return ""
	}
	return n.Status.Drain.Phase
}

func waitForDrainPhase(t *testing.T, s *controllerhelper.Suite, name string, want v1.NodeDrainPhase) {
	t.Helper()
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, name+" drain "+string(want), func() bool {
		return drainPhase(t, s, name) == want
	})
}

// moveOneOff plays the Agent and the source Node for the move currently
// backing up: the fresh backup, the stop evidence, and the cleanup afterwards.
func moveOneOff(t *testing.T, s *controllerhelper.Suite, project string) {
	t.Helper()
	waitForEvictionState(t, s, project, v1.EvictionStateBackingUp)
	agentReportsBackup(t, s, project, v1.ConditionTrue, "backup-"+project)
	sourceNodeCleansUp(t, s, project)
}

// TestCordonStopsNewPlacements validates that a cordoned node takes no new
// project and keeps the ones it has.
func TestCordonStopsNewPlacements(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "drain-a", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "drain-resident", "drain-a")
	setNodeScheduling(t, s, "drain-a", true, false)
	s.Start(t)

	ctx := context.Background()
	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "drain-newcomer"},
		Spec:       v1.ProjectSpec{Size: v1.ProjectSizeSmall},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhasePending},
	}))

	assert.Never(t, func() bool {
		p, err := s.Store.GetProject(ctx, "drain-newcomer")
		return err != nil || p.Status.NodeRef != ""
	}, 2*time.Second, 200*time.Millisecond, "a cordoned node takes no new project")

	p, err := s.Store.GetProject(ctx, "drain-resident")
	require.NoError(t, err)
	assert.Equal(t, "drain-a", p.Status.NodeRef)
	assert.Nil(t, p.Status.Eviction, "cordon alone moves nothing")
	assert.Equal(t, v1.NodeDrainPhase(""), drainPhase(t, s, "drain-a"))
}

// TestDrainEmptiesNode validates a whole drain: both projects move off one at
// a time, and the node ends Drained once it owes no cleanup.
func TestDrainEmptiesNode(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "drain-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "drain-b", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "drain-one", "drain-a")
	createRunningProject(t, s, "drain-two", "drain-a")
	s.Start(t)

	setNodeScheduling(t, s, "drain-a", true, true)
	waitForDrainPhase(t, s, "drain-a", v1.NodeDrainDraining)

	// Both are asked for at once; the eviction controller starts one at a
	// time, whichever was asked for first.
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "a move started", func() bool {
		return evictionState(t, s, "drain-one") == v1.EvictionStateBackingUp ||
			evictionState(t, s, "drain-two") == v1.EvictionStateBackingUp
	})
	first, second := "drain-one", "drain-two"
	if evictionState(t, s, first) != v1.EvictionStateBackingUp {
		first, second = second, first
	}
	moveOneOff(t, s, first)
	moveOneOff(t, s, second)

	// Drained means the node can be powered off: nothing is assigned to it
	// and it owes no cleanup. The projects may still be on their way to
	// drain-b at that point.
	waitForDrainPhase(t, s, "drain-a", v1.NodeDrainDrained)
	ctx := context.Background()
	for _, p := range []string{"drain-one", "drain-two"} {
		controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, p+" placed on drain-b", func() bool {
			got, err := s.Store.GetProject(ctx, p)
			return err == nil && got.Status.NodeRef == "drain-b"
		})
	}
}

// TestDrainWithoutRoomIsIncomplete validates that a drain with nowhere to
// put a project leaves it running, reports Incomplete, and does not
// over-commit.
func TestDrainWithoutRoomIsIncomplete(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "drain-a", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "drain-app", "drain-a")
	s.Start(t)

	setNodeScheduling(t, s, "drain-a", true, true)
	waitForDrainPhase(t, s, "drain-a", v1.NodeDrainIncomplete)

	ctx := context.Background()
	n, err := s.Store.GetNode(ctx, "drain-a")
	require.NoError(t, err)
	require.Len(t, n.Status.Drain.Blocking, 1)
	assert.Contains(t, n.Status.Drain.Blocking[0], "NoCapacity")
	found := false
	for _, c := range n.Status.Conditions {
		if c.Type == v1.ConditionTypeNodeDrainIncomplete && c.Status == v1.ConditionTrue {
			found = true
		}
	}
	assert.True(t, found, "the node carries the alert condition")

	p, err := s.Store.GetProject(ctx, "drain-app")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase, "left running where it is")
	assert.Equal(t, "drain-a", p.Status.NodeRef)
}

// TestUncordonCallsDrainOff validates calling a drain off: a move that has
// not stopped its project yet is cancelled and the drain's record cleared.
func TestUncordonCallsDrainOff(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "drain-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "drain-b", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "drain-app", "drain-a")
	s.Start(t)

	setNodeScheduling(t, s, "drain-a", true, true)
	waitForEvictionState(t, s, "drain-app", v1.EvictionStateBackingUp)

	setNodeScheduling(t, s, "drain-a", false, false)
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "drain called off", func() bool {
		return evictionState(t, s, "drain-app") == "" && drainPhase(t, s, "drain-a") == ""
	})

	p, err := s.Store.GetProject(context.Background(), "drain-app")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase)
	assert.Equal(t, "drain-a", p.Status.NodeRef, "the project stays where it is")
}

// TestCancelEvictionStopsAtTheStopEvidence validates that a move whose Agent
// has confirmed the workload stopped can no longer be cancelled.
func TestCancelEvictionStopsAtTheStopEvidence(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	drains := adapter.NewDrainStoreAdapter(s.Store)

	createRunningProject(t, s, "cancel-app", "node-a")
	require.NoError(t, drains.RequestEviction(ctx, "cancel-app", "node-a", "Drain"))
	require.NoError(t, drains.StartEvictionBackup(ctx, "cancel-app", "node-a"))
	agentReportsBackup(t, s, "cancel-app", v1.ConditionTrue, "b1")

	cancelled, err := drains.CancelEviction(ctx, "cancel-app", "node-a", "Drain")
	require.NoError(t, err)
	assert.False(t, cancelled, "the project is stopped and latched; the move must finish")
	assert.Equal(t, v1.EvictionStateBackingUp, evictionState(t, s, "cancel-app"))
}
