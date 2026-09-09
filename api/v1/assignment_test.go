package v1

import "testing"

func TestAssignmentHistoryIsValid(t *testing.T) {
	tests := []struct {
		name    string
		history AssignmentHistory
		want    bool
	}{
		{"never assigned", AssignmentHistoryNeverAssigned, true},
		{"known", AssignmentHistoryKnown, true},
		{"unknown", AssignmentHistoryUnknown, true},
		{"empty is not valid", AssignmentHistory(""), false},
		{"garbage is not valid", AssignmentHistory("Sometimes"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.history.IsValid(); got != tt.want {
				t.Fatalf("AssignmentHistory(%q).IsValid() = %v, want %v", tt.history, got, tt.want)
			}
		})
	}
}
