//go:build e2e

package controller

import (
	"context"
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
		Status: v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: node, AssignmentGeneration: 1},
	}))
}

// agentReportsBackup writes the agent's result for a move's backup, as the
// agent does through the fenced condition endpoint.
func agentReportsBackup(t *testing.T, s *controllerhelper.Suite, name string, status v1.ConditionStatus, message string) {
	t.Helper()
	ctx := context.Background()
	// The Agent confirms the workload stopped before reporting success; the
	// release gate rests on that evidence.
	if status == v1.ConditionTrue {
		p, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		require.NotNil(t, p.Status.Eviction, "the agent reports for the move it was asked to back up for")
		require.NoError(t, s.Store.RecordWorkloadStopped(ctx, name,
			store.AssignmentRef{UID: p.UID, NodeRef: p.Status.NodeRef, Generation: p.Status.AssignmentGeneration},
			&p.Status.Eviction.StateSince, time.Now().UTC()))
	}
	require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
		st.Conditions = append(st.Conditions, v1.Condition{
			Type: v1.ConditionTypeEvictionBackup, Status: status, Reason: "BackupSucceeded",
			Message: message, LastTransitionTime: time.Now().UTC(),
		})
		return nil
	}))
}

// sourceNodeCleansUp resolves the cleanup obligation a planned move left on
// the source Node, as its Agent does on its next poll: the old containers are
// removed and the local Managed data quarantined. Until then the scheduler
// may not place the Project anywhere (ADR-0024 C3). It waits for the release
// to happen first.
func sourceNodeCleansUp(t *testing.T, s *controllerhelper.Suite, name string) {
	t.Helper()
	ctx := context.Background()
	var owner store.AssignmentRef
	controllerhelper.WaitForCondition(t, 20*time.Second, 200*time.Millisecond, name+" released with a cleanup obligation", func() bool {
		p, err := s.Store.GetProject(ctx, name)
		if err != nil || p.Status.Recovery == nil {
			return false
		}
		for _, o := range p.Status.Recovery.CleanupObligations {
			if !o.Resolved() {
				owner = store.AssignmentRef{UID: o.Assignment.ProjectUID, NodeRef: o.Assignment.NodeName, Generation: o.Assignment.AssignmentGeneration}
				return true
			}
		}
		return false
	})
	now := time.Now().UTC()
	require.NoError(t, s.Store.RecordResourcesRemoved(ctx, name, owner, now))
	require.NoError(t, s.Store.RecordManagedDataQuarantined(ctx, name, owner, now))
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
	require.NoError(t, adapter.NewProjectStoreAdapter(s.Store).RequestEviction(ctx, "evict-app", "evict-a", "Drain"))

	waitForEvictionState(t, s, "evict-app", v1.EvictionStateBackingUp)
	p, err := s.Store.GetProject(ctx, "evict-app")
	require.NoError(t, err)
	assert.Equal(t, v1.ProjectPhaseRunning, p.Status.Phase, "nothing moves until the backup is in")
	assert.Equal(t, "evict-a", p.Status.NodeRef)

	agentReportsBackup(t, s, "evict-app", v1.ConditionTrue, "backup-1")
	sourceNodeCleansUp(t, s, "evict-app")

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
	require.NoError(t, evictor.RequestEviction(ctx, "evict-first", "evict-a", "Drain"))
	require.NoError(t, evictor.RequestEviction(ctx, "evict-second", "evict-a", "Drain"))

	waitForEvictionState(t, s, "evict-first", v1.EvictionStateBackingUp)
	assert.Never(t, func() bool {
		return evictionState(t, s, "evict-second") != v1.EvictionStateQueued
	}, 2*time.Second, 200*time.Millisecond, "the second move waits for the first")

	agentReportsBackup(t, s, "evict-first", v1.ConditionTrue, "backup-1")
	sourceNodeCleansUp(t, s, "evict-first")
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
	require.NoError(t, adapter.NewProjectStoreAdapter(s.Store).RequestEviction(ctx, "evict-app", "evict-only", "Drain"))

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

	err := evictor.RequestEviction(ctx, "req-app", "node-b", "Drain")
	assert.ErrorIs(t, err, store.ErrConflictState, "not running on the node named")

	require.NoError(t, evictor.RequestEviction(ctx, "req-app", "node-a", "Drain"))
	first, err := s.Store.GetProject(ctx, "req-app")
	require.NoError(t, err)
	require.NotNil(t, first.Status.Eviction)
	assert.Equal(t, v1.EvictionStateQueued, first.Status.Eviction.State)
	assert.Equal(t, "Drain", first.Status.Eviction.Reason)
	assert.Equal(t, "node-a", first.Status.Eviction.FromNode)

	require.NoError(t, evictor.RequestEviction(ctx, "req-app", "node-a", "Drain"), "asking again for the same move")
	assert.ErrorIs(t, evictor.RequestEviction(ctx, "req-app", "node-a", "FailBack"), store.ErrConflictState,
		"another trigger is told its move was not queued")
	again, err := s.Store.GetProject(ctx, "req-app")
	require.NoError(t, err)
	assert.Equal(t, "Drain", again.Status.Eviction.Reason, "asking again does not start a second move")
	assert.True(t, again.Status.Eviction.RequestedAt.Equal(first.Status.Eviction.RequestedAt))

	assert.ErrorIs(t, evictor.RequestEviction(ctx, "no-such-project", "node-a", "Drain"), store.ErrNotFound)
}

// Review A1: evidence the agent sends for a planned move counts only while
// that very move is taking its backup. Evidence that arrives after the move
// was called off would otherwise stay on a Project about to be started again,
// for a later release to rest on. Evidence sent for no move keeps the rules
// it had (a NotReady node's agent may report a Running Project stopped).
func TestStopEvidenceForAMoveNeedsThatMove(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	a := adapter.NewProjectStoreAdapter(s.Store)

	report := func(name string, forMove *time.Time) error {
		p, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		return s.Store.RecordWorkloadStopped(ctx, name,
			store.AssignmentRef{UID: p.UID, NodeRef: p.Status.NodeRef, Generation: p.Status.AssignmentGeneration}, forMove, time.Now().UTC())
	}
	since := func(name string) time.Time {
		p, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		require.NotNil(t, p.Status.Eviction)
		return p.Status.Eviction.StateSince
	}
	someMove := time.Now().UTC().Add(-time.Hour)

	createRunningProject(t, s, "no-move", "evict-a")
	assert.ErrorIs(t, report("no-move", &someMove), store.ErrConflictState, "the move was called off")

	createRunningProject(t, s, "queued", "evict-a")
	require.NoError(t, a.RequestEviction(ctx, "queued", "evict-a", "Drain"))
	queuedSince := since("queued")
	assert.ErrorIs(t, report("queued", &queuedSince), store.ErrConflictState, "the move has not asked for the backup yet")

	createRunningProject(t, s, "backing-up", "evict-a")
	require.NoError(t, a.RequestEviction(ctx, "backing-up", "evict-a", "Drain"))
	require.NoError(t, a.StartEvictionBackup(ctx, "backing-up", "evict-a"))
	assert.ErrorIs(t, report("backing-up", &someMove), store.ErrConflictState, "an earlier move's evidence")
	current := since("backing-up")
	assert.NoError(t, report("backing-up", &current))

	createRunningProject(t, s, "untagged", "evict-a")
	assert.NoError(t, report("untagged", nil), "evidence for no move keeps its old rules")
}

// Review A1: a move is not abandoned once its stop evidence is in, even if the
// decision to abandon it was made on an older read.
func TestEndEvictionKeepsAMoveWhoseStopIsReported(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	a := adapter.NewProjectStoreAdapter(s.Store)

	createRunningProject(t, s, "evict-app", "evict-a")
	require.NoError(t, a.RequestEviction(ctx, "evict-app", "evict-a", "Drain"))
	require.NoError(t, a.StartEvictionBackup(ctx, "evict-app", "evict-a"))
	snapshots, err := a.ListEvictions(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	stale := snapshots[0]
	require.False(t, stale.WorkloadStopped)

	agentReportsBackup(t, s, "evict-app", v1.ConditionTrue, "b-1")

	err = a.EndEviction(ctx, stale, &controller.EvictionFailure{Reason: controller.EvictionFailedAgentDidNotStart, Message: "too slow"})
	assert.ErrorIs(t, err, store.ErrConflictState)
	assert.Equal(t, v1.EvictionStateBackingUp, evictionState(t, s, "evict-app"), "the move goes on")

	// A snapshot of another lifetime writes nothing.
	other := *stale
	other.UID = "uid-of-another-lifetime"
	assert.NoError(t, a.EndEviction(ctx, &other, nil))
	assert.Equal(t, v1.EvictionStateBackingUp, evictionState(t, s, "evict-app"))

	// Stamping the start is a one-off for this move.
	first := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, a.MarkEvictionAgentStarted(ctx, stale, first))
	require.NoError(t, a.MarkEvictionAgentStarted(ctx, stale, first.Add(time.Hour)))
	p, err := s.Store.GetProject(ctx, "evict-app")
	require.NoError(t, err)
	require.NotNil(t, p.Status.Eviction.AgentStartedAt)
	assert.True(t, p.Status.Eviction.AgentStartedAt.Equal(first))
}
