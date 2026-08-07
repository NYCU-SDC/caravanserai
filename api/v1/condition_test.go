package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
