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
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClearRescheduleClocks carries the two guards that keep a late cleanup from
// deleting the next outage's clock. They live inside the adapter's
// read-modify-write against the real store, so a controller test using a fake
// proves nothing about them however carefully it stages the race — the fake is
// where the guards would be duplicated, not where they are.
func TestClearRescheduleClocksGuards(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(s.Store)

	recoveryBeat := time.Now().UTC().Truncate(time.Millisecond)

	clock := func(t time.Time) v1.Condition {
		return v1.Condition{
			Type:               v1.ConditionTypeNotReadyAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			LastTransitionTime: t,
		}
	}
	phase := v1.Condition{
		Type:               v1.ConditionTypePhase,
		Status:             v1.ConditionTrue,
		Reason:             "ContainersRunning",
		LastTransitionTime: recoveryBeat,
	}

	// create writes a Running Project on nodeRef carrying phase plus the given
	// clock, and returns its name.
	create := func(t *testing.T, name, nodeRef string, clockAt time.Time) string {
		t.Helper()
		p := &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
			ObjectMeta: v1.ObjectMeta{Name: name},
			Status: v1.ProjectStatus{
				Phase:      v1.ProjectPhaseRunning,
				NodeRef:    nodeRef,
				Conditions: []v1.Condition{phase, clock(clockAt)},
			},
		}
		require.NoError(t, s.Store.CreateProject(ctx, p))
		// CreateProject may not persist status, so assert it explicitly.
		require.NoError(t, s.Store.UpdateProjectStatus(ctx, name, p.Status))
		return name
	}

	conditionTypes := func(t *testing.T, name string) []v1.ConditionType {
		t.Helper()
		got, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		types := make([]v1.ConditionType, 0, len(got.Status.Conditions))
		for _, c := range got.Status.Conditions {
			types = append(types, c.Type)
		}
		return types
	}

	t.Run("a clock older than the recovery heartbeat is removed", func(t *testing.T) {
		name := create(t, "clk-stale", "node-a", recoveryBeat.Add(-time.Minute))

		require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, name, "node-a"), recoveryBeat))

		assert.Equal(t, []v1.ConditionType{v1.ConditionTypePhase}, conditionTypes(t, name))
	})

	t.Run("a clock newer than the recovery heartbeat survives", func(t *testing.T) {
		// The next outage began after this cleanup was decided on. Deleting
		// its clock would hand the Project an extra full grace period.
		name := create(t, "clk-fresh", "node-a", recoveryBeat.Add(2*time.Minute))

		require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, name, "node-a"), recoveryBeat))

		assert.Equal(t,
			[]v1.ConditionType{v1.ConditionTypePhase, v1.ConditionTypeNotReadyAt},
			conditionTypes(t, name))
	})

	// A refused release leaves TakeoverRequired, which belongs to the same
	// outage as the clocks and must go with them when the node recovers — a
	// Scheduled Project carries it with no clock at all.
	t.Run("TakeoverRequired follows the same guard as the clocks", func(t *testing.T) {
		takeover := func(at time.Time) v1.Condition {
			return v1.Condition{
				Type:               v1.ConditionTypeTakeoverRequired,
				Status:             v1.ConditionTrue,
				Reason:             "WorkloadStopUnconfirmed",
				LastTransitionTime: at,
			}
		}
		stale := create(t, "clk-takeover-stale", "node-a", recoveryBeat.Add(-time.Minute))
		fresh := create(t, "clk-takeover-fresh", "node-a", recoveryBeat.Add(-time.Minute))
		for name, at := range map[string]time.Time{
			stale: recoveryBeat.Add(-time.Minute),
			fresh: recoveryBeat.Add(2 * time.Minute),
		} {
			require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, name, func(st *v1.ProjectStatus) error {
				st.Conditions = append(st.Conditions, takeover(at))
				return nil
			}))
			require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, name, "node-a"), recoveryBeat))
		}

		assert.Equal(t, []v1.ConditionType{v1.ConditionTypePhase}, conditionTypes(t, stale))
		assert.Equal(t,
			[]v1.ConditionType{v1.ConditionTypePhase, v1.ConditionTypeTakeoverRequired},
			conditionTypes(t, fresh))
	})

	t.Run("a Project that has moved to another node is untouched", func(t *testing.T) {
		name := create(t, "clk-moved", "node-b", recoveryBeat.Add(-time.Minute))

		// node-a's cleanup, arriving after the Project was reassigned.
		require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, name, "node-a"), recoveryBeat))

		assert.Equal(t,
			[]v1.ConditionType{v1.ConditionTypePhase, v1.ConditionTypeNotReadyAt},
			conditionTypes(t, name))
	})
}

// SetNotReadyAt has to move the timestamp on every call. Every field but the
// timestamp is a fixed string, so an unchanged-content check in
// UpsertCondition would keep the previous incident's start time and the clock
// would never restart — invisibly to any test whose fake replaces it.
func TestSetNotReadyAtRestartsTheClockInTheStore(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(s.Store)

	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "clk-restart"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: "node-a"},
	}))

	first := time.Now().UTC().Truncate(time.Millisecond)
	second := first.Add(4 * time.Hour)

	readClock := func(t *testing.T, condType v1.ConditionType) time.Time {
		t.Helper()
		got, err := s.Store.GetProject(ctx, "clk-restart")
		require.NoError(t, err)
		for _, c := range got.Status.Conditions {
			if c.Type == condType {
				return c.LastTransitionTime.UTC()
			}
		}
		t.Fatalf("no %s condition on the project", condType)
		return time.Time{}
	}

	require.NoError(t, projects.SetNotReadyAt(ctx, snapshot(t, s, "clk-restart", "node-a"), first, first.Add(-time.Minute)))
	require.WithinDuration(t, first, readClock(t, v1.ConditionTypeNotReadyAt), time.Millisecond)

	require.NoError(t, projects.SetNotReadyAt(ctx, snapshot(t, s, "clk-restart", "node-a"), second, first.Add(time.Hour)))
	assert.WithinDuration(t, second, readClock(t, v1.ConditionTypeNotReadyAt), time.Millisecond,
		"a second incident must start its own clock, not inherit the first's")

	require.NoError(t, forcePhase(t, s, "clk-restart", v1.ProjectPhaseTerminating))
	require.NoError(t, projects.SetTerminatingAt(ctx, snapshot(t, s, "clk-restart", "node-a"), first, first.Add(-time.Minute)))
	require.NoError(t, projects.SetTerminatingAt(ctx, snapshot(t, s, "clk-restart", "node-a"), second, first.Add(time.Hour)))
	assert.WithinDuration(t, second, readClock(t, v1.ConditionTypeTerminatingAt), time.Millisecond,
		"the same holds for the termination clock")
}

// An expired termination timeout must leave the record in place: only the
// Project's node can confirm its resources are gone. The mark is per outage
// like the clocks, so it restamps on a new outage and goes with them when the
// node returns.
func TestTerminationBlockedKeepsTheProjectTerminating(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)

	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(s.Store)

	create := func(t *testing.T, name string, phase v1.ProjectPhase) {
		t.Helper()
		p := &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
			ObjectMeta: v1.ObjectMeta{Name: name},
			Status:     v1.ProjectStatus{Phase: phase, NodeRef: "node-a"},
		}
		require.NoError(t, s.Store.CreateProject(ctx, p))
		require.NoError(t, s.Store.UpdateProjectStatus(ctx, name, p.Status))
	}
	blockedAt := func(t *testing.T, name string) (time.Time, bool) {
		t.Helper()
		got, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		for _, c := range got.Status.Conditions {
			if c.Type == v1.ConditionTypeTerminationBlocked {
				return c.LastTransitionTime.UTC(), true
			}
		}
		return time.Time{}, false
	}

	first := time.Now().UTC().Truncate(time.Millisecond)
	second := first.Add(4 * time.Hour)

	t.Run("the phase stays Terminating and a new outage restamps the mark", func(t *testing.T) {
		create(t, "blocked", v1.ProjectPhaseTerminating)

		require.NoError(t, projects.SetTerminationBlocked(ctx, snapshot(t, s, "blocked", "node-a"), first, first.Add(-time.Minute)))
		got, err := s.Store.GetProject(ctx, "blocked")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseTerminating, got.Status.Phase,
			"an expired timeout must not finalize the project")
		at, ok := blockedAt(t, "blocked")
		require.True(t, ok)
		assert.WithinDuration(t, first, at, time.Millisecond)

		require.NoError(t, projects.SetTerminationBlocked(ctx, snapshot(t, s, "blocked", "node-a"), second, first.Add(time.Hour)))
		at, _ = blockedAt(t, "blocked")
		assert.WithinDuration(t, second, at, time.Millisecond)
	})

	t.Run("a Project that left Terminating is not marked", func(t *testing.T) {
		create(t, "not-terminating", v1.ProjectPhaseRunning)

		require.NoError(t, projects.SetTerminationBlocked(ctx, snapshot(t, s, "not-terminating", "node-a"), first, first.Add(-time.Minute)))
		_, ok := blockedAt(t, "not-terminating")
		assert.False(t, ok)
	})

	t.Run("the mark goes with the clocks when the node returns", func(t *testing.T) {
		create(t, "recovered", v1.ProjectPhaseTerminating)
		require.NoError(t, projects.SetTerminatingAt(ctx, snapshot(t, s, "recovered", "node-a"), first.Add(-time.Hour), first.Add(-2*time.Hour)))
		require.NoError(t, projects.SetTerminationBlocked(ctx, snapshot(t, s, "recovered", "node-a"), first, first.Add(-time.Minute)))

		// A cleanup decided on a heartbeat older than the mark leaves it:
		// the mark belongs to an outage that began after that heartbeat.
		require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, "recovered", "node-a"), first.Add(-time.Minute)))
		_, ok := blockedAt(t, "recovered")
		require.True(t, ok)

		require.NoError(t, projects.ClearRescheduleClocks(ctx, snapshot(t, s, "recovered", "node-a"), first.Add(time.Minute)))
		_, ok = blockedAt(t, "recovered")
		assert.False(t, ok)
		got, err := s.Store.GetProject(ctx, "recovered")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseTerminating, got.Status.Phase)
	})
}

// snapshot is what the rescheduler would have read for name: its lifetime and
// generation as stored now, on nodeRef.
func snapshot(t *testing.T, s *controllerhelper.Suite, name, nodeRef string) *controller.ProjectSnapshot {
	t.Helper()
	got, err := s.Store.GetProject(context.Background(), name)
	require.NoError(t, err)
	return &controller.ProjectSnapshot{
		Name: name, UID: got.ObjectMeta.UID, Phase: got.Status.Phase,
		NodeRef: nodeRef, AssignmentGeneration: got.Status.AssignmentGeneration,
	}
}

func forcePhase(t *testing.T, s *controllerhelper.Suite, name string, phase v1.ProjectPhase) error {
	t.Helper()
	return testhelper.ForcePhase(context.Background(), s.Store, name, phase, "Test", "")
}

// CARA-81 stage 3: a controller write lands only on what its decision was
// about. Each case reads a snapshot, changes the Project the way a race
// between the controller's read and its write would, and then makes the
// write the controller decided on from the old snapshot.
func TestReschedulerWritesFollowTheDecision(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	ctx := context.Background()
	projects := adapter.NewProjectStoreAdapter(s.Store)
	heartbeat := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	now := heartbeat.Add(30 * time.Second)

	running := func(t *testing.T, name string) {
		t.Helper()
		_ = s.Store.DeleteProject(ctx, name)
		require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
			ObjectMeta: v1.ObjectMeta{Name: name},
			Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
			Status:     v1.InitialProjectStatus(),
		}))
		require.NoError(t, projects.SetProjectScheduled(ctx, name, testhelper.UID(t, s.Store, name), "node-a"))
		require.NoError(t, forcePhase(t, s, name, v1.ProjectPhaseRunning))
	}
	notReadyAt := func(t *testing.T, name string) (time.Time, bool) {
		t.Helper()
		got, err := s.Store.GetProject(ctx, name)
		require.NoError(t, err)
		for _, c := range got.Status.Conditions {
			if c.Type == v1.ConditionTypeNotReadyAt {
				return c.LastTransitionTime.UTC(), true
			}
		}
		return time.Time{}, false
	}

	t.Run("a same-name Project created since gets nothing", func(t *testing.T) {
		running(t, "race-recreated")
		old := snapshot(t, s, "race-recreated", "node-a")
		running(t, "race-recreated")
		require.NotEqual(t, old.UID, testhelper.UID(t, s.Store, "race-recreated"))

		require.NoError(t, projects.SetNotReadyAt(ctx, old, now, heartbeat))
		_, has := notReadyAt(t, "race-recreated")
		assert.False(t, has, "the clock was decided for the old lifetime")
	})

	t.Run("a Project moved to a new generation gets nothing", func(t *testing.T) {
		running(t, "race-new-generation")
		old := snapshot(t, s, "race-new-generation", "node-a")
		require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "race-new-generation", func(st *v1.ProjectStatus) error {
			st.AssignmentGeneration++
			return nil
		}))

		require.NoError(t, projects.SetNotReadyAt(ctx, old, now, heartbeat))
		_, has := notReadyAt(t, "race-new-generation")
		assert.False(t, has)
	})

	t.Run("a Project moved to another Node gets nothing", func(t *testing.T) {
		running(t, "race-new-node")
		old := snapshot(t, s, "race-new-node", "node-a")
		require.NoError(t, s.Store.UpdateProjectStatusWithRetry(ctx, "race-new-node", func(st *v1.ProjectStatus) error {
			st.NodeRef = "node-b"
			return nil
		}))

		require.NoError(t, projects.SetNotReadyAt(ctx, old, now, heartbeat))
		_, has := notReadyAt(t, "race-new-node")
		assert.False(t, has)
	})

	t.Run("a clock already started for this outage is not moved back", func(t *testing.T) {
		running(t, "race-same-outage")
		snap := snapshot(t, s, "race-same-outage", "node-a")
		started := heartbeat.Add(10 * time.Second)
		require.NoError(t, projects.SetNotReadyAt(ctx, snap, started, heartbeat))

		// A pass that read the Project before that clock existed lands late.
		require.NoError(t, projects.SetNotReadyAt(ctx, snap, now, heartbeat))
		at, _ := notReadyAt(t, "race-same-outage")
		assert.WithinDuration(t, started, at, time.Millisecond)
	})

	t.Run("a decision from an earlier outage does not overwrite the new one's clock", func(t *testing.T) {
		running(t, "race-new-outage")
		snap := snapshot(t, s, "race-new-outage", "node-a")
		// The Node recovered and failed again; another pass started the second
		// outage's clock after the new heartbeat.
		secondBeat := heartbeat.Add(time.Hour)
		secondClock := secondBeat.Add(time.Minute)
		require.NoError(t, projects.SetNotReadyAt(ctx, snap, secondClock, secondBeat))

		// The pass that judged the first outage lands now.
		require.NoError(t, projects.SetNotReadyAt(ctx, snap, now, heartbeat))
		at, _ := notReadyAt(t, "race-new-outage")
		assert.WithinDuration(t, secondClock, at, time.Millisecond,
			"moving it back would cut the second outage's grace period short")
	})

	t.Run("a phase the decision was not about gets nothing", func(t *testing.T) {
		running(t, "race-left-running")
		snap := snapshot(t, s, "race-left-running", "node-a")
		require.NoError(t, forcePhase(t, s, "race-left-running", v1.ProjectPhaseFailed))

		require.NoError(t, projects.SetNotReadyAt(ctx, snap, now, heartbeat))
		_, has := notReadyAt(t, "race-left-running")
		assert.False(t, has)
	})

	t.Run("the scheduler cannot assign a same-name Project created since", func(t *testing.T) {
		_ = s.Store.DeleteProject(ctx, "race-schedule")
		create := func() string {
			require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
				TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
				ObjectMeta: v1.ObjectMeta{Name: "race-schedule"},
				Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
				Status:     v1.InitialProjectStatus(),
			}))
			return testhelper.UID(t, s.Store, "race-schedule")
		}
		oldUID := create()
		require.NoError(t, s.Store.DeleteProject(ctx, "race-schedule"))
		create()

		err := projects.SetProjectScheduled(ctx, "race-schedule", oldUID, "node-a")
		assert.ErrorIs(t, err, store.ErrStaleAssignment)
		got, getErr := s.Store.GetProject(ctx, "race-schedule")
		require.NoError(t, getErr)
		assert.Equal(t, v1.ProjectPhasePending, got.Status.Phase)
		assert.Zero(t, got.Status.AssignmentGeneration)
	})
}
