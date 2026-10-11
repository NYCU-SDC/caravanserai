//go:build e2e

package controller

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/store"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createRunningProject creates a Project Running on node with one Managed
// volume, so a move has data to carry.
func createRunningProject(t *testing.T, s *controllerhelper.Suite, name, node string) {
	t.Helper()
	require.NoError(t, s.Store.CreateProject(context.Background(), &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Spec: v1.ProjectSpec{
			Size:    v1.ProjectSizeSmall,
			Volumes: []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
		},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhaseRunning, NodeRef: node, AssignmentGeneration: 1,
			Conditions: []v1.Condition{{
				Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: "ContainersRunning", LastTransitionTime: longStable,
			}},
		},
	}))
}

// agentReportsBackup writes the agent's result for a move's backup, as the
// agent does through the fenced condition endpoint.
func agentReportsBackup(t *testing.T, s *controllerhelper.Suite, name string, status v1.ConditionStatus, message string) {
	t.Helper()
	require.NoError(t, s.Store.UpdateProjectStatusWithRetry(context.Background(), name, func(st *v1.ProjectStatus) error {
		st.Conditions = append(st.Conditions, v1.Condition{
			Type: v1.ConditionTypeEvictionBackup, Status: status, Reason: "BackupSucceeded",
			Message: message, LastTransitionTime: time.Now().UTC(),
		})
		return nil
	}))
}

func evictionState(t *testing.T, s *controllerhelper.Suite, name string) v1.EvictionState {
	t.Helper()
	p, err := s.Store.GetProject(context.Background(), name)
	require.NoError(t, err)
	if p.Status.Eviction == nil {
		return ""
	}
	return p.Status.Eviction.State
}

func waitForEvictionState(t *testing.T, s *controllerhelper.Suite, name string, want v1.EvictionState) {
	t.Helper()
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, name+" eviction "+string(want), func() bool {
		return evictionState(t, s, name) == want
	})
}

// TestPlannedMoveEndToEnd validates a whole planned move: queued, the agent
// asked for a fresh backup, back to Pending once it reports, placed by the
// scheduler on a node other than the source, done.
func TestPlannedMoveEndToEnd(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	// The source is primary and sorts first, so the scheduler would choose it
	// again if the move did not exclude it.
	createLedgerNode(t, s, "evict-a", "primary", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "evict-b", "backup", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "evict-app", "evict-a")
	s.Start(t)

	ctx := context.Background()
	require.NoError(t, adapter.NewProjectStoreAdapter(s.Store).RequestEviction(ctx, "evict-app", "evict-a", "Drain", "", nil))

	waitForEvictionState(t, s, "evict-app", v1.EvictionStateBackingUp)
	p, err := s.Store.GetProject(ctx, "evict-app")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase, "nothing moves until the backup is in")
	assert.Equal(t, "evict-a", p.Status.NodeRef)

	agentReportsBackup(t, s, "evict-app", v1.ConditionTrue, "backup-1")

	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "moved to evict-b and done", func() bool {
		p, err := s.Store.GetProject(ctx, "evict-app")
		return err == nil && p.Status.NodeRef == "evict-b" && p.Status.Eviction == nil
	})
	p, err = s.Store.GetProject(ctx, "evict-app")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseScheduled, p.Status.Phase)
	for _, c := range p.Status.Conditions {
		assert.NotEqual(t, v1.ConditionTypeEvictionFailed, c.Type)
		assert.NotEqual(t, v1.ConditionTypeEvictionBackup, c.Type, "the move's backup result is cleared with it")
	}
}

// TestPlannedMovesRunOneAtATime validates the cluster-wide limit: the second
// move waits until the first has been placed.
func TestPlannedMovesRunOneAtATime(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "evict-a", "", controllerhelper.RoomyAllocatable(), false)
	createLedgerNode(t, s, "evict-b", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "evict-first", "evict-a")
	createRunningProject(t, s, "evict-second", "evict-a")
	s.Start(t)

	ctx := context.Background()
	evictor := adapter.NewProjectStoreAdapter(s.Store)
	require.NoError(t, evictor.RequestEviction(ctx, "evict-first", "evict-a", "Drain", "", nil))
	require.NoError(t, evictor.RequestEviction(ctx, "evict-second", "evict-a", "Drain", "", nil))

	waitForEvictionState(t, s, "evict-first", v1.EvictionStateBackingUp)
	assert.Never(t, func() bool {
		return evictionState(t, s, "evict-second") != v1.EvictionStateQueued
	}, 2*time.Second, 200*time.Millisecond, "the second move waits for the first")

	agentReportsBackup(t, s, "evict-first", v1.ConditionTrue, "backup-1")
	waitForEvictionState(t, s, "evict-second", v1.EvictionStateBackingUp)
	assert.Equal(t, v1.EvictionState(""), evictionState(t, s, "evict-first"), "the first is done before the second starts")
}

// TestPlannedMoveRefusedWithoutCapacity validates that a move with nowhere to
// go is abandoned before anything is stopped.
func TestPlannedMoveRefusedWithoutCapacity(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	createLedgerNode(t, s, "evict-only", "", controllerhelper.RoomyAllocatable(), false)
	createRunningProject(t, s, "evict-app", "evict-only")
	s.Start(t)

	ctx := context.Background()
	require.NoError(t, adapter.NewProjectStoreAdapter(s.Store).RequestEviction(ctx, "evict-app", "evict-only", "Drain", "", nil))

	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, "EvictionFailed NoCapacity", func() bool {
		p, err := s.Store.GetProject(ctx, "evict-app")
		if err != nil {
			return false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == v1.ConditionTypeEvictionFailed && c.Reason == "NoCapacity" {
				return true
			}
		}
		return false
	})
	p, err := s.Store.GetProject(ctx, "evict-app")
	require.NoError(t, err)
	assert.Nil(t, p.Status.Eviction)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase, "a refused move leaves the project running where it is")
	assert.Equal(t, "evict-only", p.Status.NodeRef)
}

// TestRequestEviction validates the entry point drain and fail-back use.
func TestRequestEviction(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	evictor := adapter.NewProjectStoreAdapter(s.Store)

	createRunningProject(t, s, "req-app", "node-a")

	err := evictor.RequestEviction(ctx, "req-app", "node-b", "Drain", "", nil)
	assert.ErrorIs(t, err, store.ErrConflictState, "not running on the node named")

	require.NoError(t, evictor.RequestEviction(ctx, "req-app", "node-a", "Drain", "", nil))
	first, err := s.Store.GetProject(ctx, "req-app")
	require.NoError(t, err)
	require.NotNil(t, first.Status.Eviction)
	assert.Equal(t, v1.EvictionStateQueued, first.Status.Eviction.State)
	assert.Equal(t, "Drain", first.Status.Eviction.Reason)
	assert.Equal(t, "node-a", first.Status.Eviction.FromNode)

	require.NoError(t, evictor.RequestEviction(ctx, "req-app", "node-a", "FailBack", "", nil))
	again, err := s.Store.GetProject(ctx, "req-app")
	require.NoError(t, err)
	assert.Equal(t, "Drain", again.Status.Eviction.Reason, "asking again does not start a second move")
	assert.True(t, again.Status.Eviction.RequestedAt.Equal(first.Status.Eviction.RequestedAt))

	assert.ErrorIs(t, evictor.RequestEviction(ctx, "no-such-project", "node-a", "Drain", "", nil), store.ErrNotFound)
}
