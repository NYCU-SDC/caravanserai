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

		require.NoError(t, projects.ClearRescheduleClocks(ctx, name, "node-a", recoveryBeat))

		assert.Equal(t, []v1.ConditionType{v1.ConditionTypePhase}, conditionTypes(t, name))
	})

	t.Run("a clock newer than the recovery heartbeat survives", func(t *testing.T) {
		// The next outage began after this cleanup was decided on. Deleting
		// its clock would hand the Project an extra full grace period.
		name := create(t, "clk-fresh", "node-a", recoveryBeat.Add(2*time.Minute))

		require.NoError(t, projects.ClearRescheduleClocks(ctx, name, "node-a", recoveryBeat))

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
			require.NoError(t, projects.ClearRescheduleClocks(ctx, name, "node-a", recoveryBeat))
		}

		assert.Equal(t, []v1.ConditionType{v1.ConditionTypePhase}, conditionTypes(t, stale))
		assert.Equal(t,
			[]v1.ConditionType{v1.ConditionTypePhase, v1.ConditionTypeTakeoverRequired},
			conditionTypes(t, fresh))
	})

	t.Run("a Project that has moved to another node is untouched", func(t *testing.T) {
		name := create(t, "clk-moved", "node-b", recoveryBeat.Add(-time.Minute))

		// node-a's cleanup, arriving after the Project was reassigned.
		require.NoError(t, projects.ClearRescheduleClocks(ctx, name, "node-a", recoveryBeat))

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

	require.NoError(t, projects.SetNotReadyAt(ctx, "clk-restart", first))
	require.WithinDuration(t, first, readClock(t, v1.ConditionTypeNotReadyAt), time.Millisecond)

	require.NoError(t, projects.SetNotReadyAt(ctx, "clk-restart", second))
	assert.WithinDuration(t, second, readClock(t, v1.ConditionTypeNotReadyAt), time.Millisecond,
		"a second incident must start its own clock, not inherit the first's")

	require.NoError(t, projects.SetTerminatingAt(ctx, "clk-restart", first))
	require.NoError(t, projects.SetTerminatingAt(ctx, "clk-restart", second))
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

		require.NoError(t, projects.SetTerminationBlocked(ctx, "blocked", "node-a", first))
		got, err := s.Store.GetProject(ctx, "blocked")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseTerminating, got.Status.Phase,
			"an expired timeout must not finalize the project")
		at, ok := blockedAt(t, "blocked")
		require.True(t, ok)
		assert.WithinDuration(t, first, at, time.Millisecond)

		require.NoError(t, projects.SetTerminationBlocked(ctx, "blocked", "node-a", second))
		at, _ = blockedAt(t, "blocked")
		assert.WithinDuration(t, second, at, time.Millisecond)
	})

	t.Run("a Project that left Terminating is not marked", func(t *testing.T) {
		create(t, "not-terminating", v1.ProjectPhaseRunning)

		require.NoError(t, projects.SetTerminationBlocked(ctx, "not-terminating", "node-a", first))
		_, ok := blockedAt(t, "not-terminating")
		assert.False(t, ok)
	})

	t.Run("the mark goes with the clocks when the node returns", func(t *testing.T) {
		create(t, "recovered", v1.ProjectPhaseTerminating)
		require.NoError(t, projects.SetTerminatingAt(ctx, "recovered", first.Add(-time.Hour)))
		require.NoError(t, projects.SetTerminationBlocked(ctx, "recovered", "node-a", first))

		// A cleanup decided on a heartbeat older than the mark leaves it:
		// the mark belongs to an outage that began after that heartbeat.
		require.NoError(t, projects.ClearRescheduleClocks(ctx, "recovered", "node-a", first.Add(-time.Minute)))
		_, ok := blockedAt(t, "recovered")
		require.True(t, ok)

		require.NoError(t, projects.ClearRescheduleClocks(ctx, "recovered", "node-a", first.Add(time.Minute)))
		_, ok = blockedAt(t, "recovered")
		assert.False(t, ok)
		got, err := s.Store.GetProject(ctx, "recovered")
		require.NoError(t, err)
		assert.Equal(t, v1.ProjectPhaseTerminating, got.Status.Phase)
	})
}
