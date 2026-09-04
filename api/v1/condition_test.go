package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsMaintenanceActive(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	maintenance := func(status ConditionStatus, age time.Duration) []Condition {
		return []Condition{
			{Type: ConditionTypePhase, Status: ConditionTrue, LastTransitionTime: now},
			{Type: ConditionTypeMaintenance, Status: status, LastTransitionTime: now.Add(-age)},
		}
	}

	tests := []struct {
		name       string
		conditions []Condition
		want       bool
	}{
		{
			name:       "fresh and True is active",
			conditions: maintenance(ConditionTrue, time.Minute),
			want:       true,
		},
		{
			name:       "just inside the staleness window is active",
			conditions: maintenance(ConditionTrue, MaintenanceStaleAfter-time.Second),
			want:       true,
		},
		{
			// An agent that dies mid-backup leaves the condition behind with
			// nothing to clear it; readers must not treat the Project as
			// under maintenance forever.
			name:       "past the staleness window is ignored",
			conditions: maintenance(ConditionTrue, MaintenanceStaleAfter+time.Second),
			want:       false,
		},
		{
			name:       "explicitly False is not active",
			conditions: maintenance(ConditionFalse, time.Minute),
			want:       false,
		},
		{
			name:       "absent condition is not active",
			conditions: []Condition{{Type: ConditionTypePhase, Status: ConditionTrue, LastTransitionTime: now}},
			want:       false,
		},
		{
			name:       "no conditions at all is not active",
			conditions: nil,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMaintenanceActive(tt.conditions, now))
		})
	}
}

func TestUpsertConditionPreservesTimestampsWhenNothingChanged(t *testing.T) {
	began := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	later := began.Add(90 * time.Second)

	existing := []Condition{{
		Type:               ConditionTypePhase,
		Status:             ConditionTrue,
		Reason:             "ContainersRunning",
		Message:            "All containers running",
		LastHeartbeatTime:  began,
		LastTransitionTime: began,
	}}

	t.Run("identical re-assertion keeps the original timestamps", func(t *testing.T) {
		got := UpsertCondition(append([]Condition(nil), existing...), Condition{
			Type:               ConditionTypePhase,
			Status:             ConditionTrue,
			Reason:             "ContainersRunning",
			Message:            "All containers running",
			LastHeartbeatTime:  later,
			LastTransitionTime: later,
		}, false)

		require.Len(t, got, 1)
		assert.Equal(t, began, got[0].LastTransitionTime,
			"an unchanged re-assertion moved the transition time; downstream no-op guards cannot fire")
		assert.Equal(t, began, got[0].LastHeartbeatTime)
	})

	for _, tc := range []struct {
		name string
		next Condition
	}{
		{"status changed", Condition{Type: ConditionTypePhase, Status: ConditionFalse, Reason: "ContainersRunning", Message: "All containers running"}},
		{"reason changed", Condition{Type: ConditionTypePhase, Status: ConditionTrue, Reason: "ContainerExited", Message: "All containers running"}},
		{"message changed", Condition{Type: ConditionTypePhase, Status: ConditionTrue, Reason: "ContainersRunning", Message: "exit 1"}},
	} {
		t.Run(tc.name+" moves the transition time", func(t *testing.T) {
			next := tc.next
			next.LastTransitionTime = later
			next.LastHeartbeatTime = later

			got := UpsertCondition(append([]Condition(nil), existing...), next, false)

			require.Len(t, got, 1)
			assert.Equal(t, later, got[0].LastTransitionTime,
				"a real change must record when it happened")
		})
	}

	t.Run("a different type is appended", func(t *testing.T) {
		got := UpsertCondition(append([]Condition(nil), existing...), Condition{
			Type: ConditionTypeMaintenance, Status: ConditionTrue, LastTransitionTime: later,
		}, false)

		require.Len(t, got, 2)
		assert.Equal(t, began, got[0].LastTransitionTime, "the untouched condition must not move")
		assert.Equal(t, later, got[1].LastTransitionTime)
	})

	t.Run("applying twice matches applying once", func(t *testing.T) {
		next := Condition{
			Type: ConditionTypePhase, Status: ConditionFalse,
			Reason: "ContainerExited", Message: "exit 1", LastTransitionTime: later,
		}
		once := UpsertCondition(append([]Condition(nil), existing...), next, false)

		// A retry re-runs the mutation against the state the first run produced.
		// The second application must be a no-op, or a conflict would quietly
		// rewrite the timestamp.
		twice := UpsertCondition(append([]Condition(nil), once...), next, false)

		assert.Equal(t, once, twice)
	})
}

// The Phase condition cannot see the value that actually moved: its own Status
// is always True, and the phase lives in ProjectStatus. A caller that knows the
// phase changed says so, and the timestamp must follow.
func TestUpsertConditionHonoursTransitionedFlag(t *testing.T) {
	began := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	later := began.Add(90 * time.Second)

	// Exactly the shape a node flapping during scheduling produces:
	// SetProjectPending -> SetProjectScheduled (conditions untouched) ->
	// SetProjectPending again, with the reason unchanged throughout.
	stale := []Condition{{
		Type:               ConditionTypePhase,
		Status:             ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady; project reset to Pending for rescheduling",
		LastTransitionTime: began,
	}}
	incoming := Condition{
		Type:               ConditionTypePhase,
		Status:             ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady; project reset to Pending for rescheduling",
		LastTransitionTime: later,
	}

	got := UpsertCondition(append([]Condition(nil), stale...), incoming, true)
	require.Len(t, got, 1)
	assert.Equal(t, later, got[0].LastTransitionTime,
		"the phase changed, so the condition must record when — even though its own fields match")

	got = UpsertCondition(append([]Condition(nil), stale...), incoming, false)
	require.Len(t, got, 1)
	assert.Equal(t, began, got[0].LastTransitionTime,
		"nothing changed inside or outside the condition; the timestamp must not move")
}
