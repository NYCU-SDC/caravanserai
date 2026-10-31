package v1

import (
	"strings"
	"testing"
	"time"
)

func obligation(node string, generation int64, removed bool) CleanupObligation {
	o := CleanupObligation{
		Assignment:           AssignmentIdentity{ProjectUID: "uid-1", NodeName: node, AssignmentGeneration: generation},
		ReleasedAt:           time.Now(),
		ReleaseAuthorization: ReleaseByDeletion,
	}
	if removed {
		o.ResourcesRemoved = &ResourcesRemovedEvidence{ObservedAt: time.Now()}
	}
	return o
}

func terminated(obligations ...CleanupObligation) *Project {
	return &Project{Status: ProjectStatus{
		Phase:                ProjectPhaseTerminated,
		NodeRef:              "node-a",
		AssignmentGeneration: 3,
		AssignmentHistory:    AssignmentHistoryKnown,
		Recovery:             &ProjectRecoveryStatus{CleanupObligations: obligations},
	}}
}

func TestFinalizationBlocker(t *testing.T) {
	tests := []struct {
		name    string
		project *Project
		// wantBlockedBy is a substring of the reason; empty means allowed.
		wantBlockedBy string
	}{
		{
			name:    "Terminated with its own obligation resolved",
			project: terminated(obligation("node-a", 3, true)),
		},
		{
			name:    "Terminated with every obligation resolved",
			project: terminated(obligation("node-b", 1, true), obligation("node-a", 3, true)),
		},
		{
			name:          "not Terminated",
			project:       &Project{Status: ProjectStatus{Phase: ProjectPhaseTerminating}},
			wantBlockedBy: "not Terminated",
		},
		{
			// The empty list resolves trivially; the reporting assignment's
			// own evidence is what keeps it from passing.
			name:          "Terminated with no obligations at all",
			project:       terminated(),
			wantBlockedBy: "has not recorded ResourcesRemoved",
		},
		{
			name:          "Terminated with no recovery status",
			project:       &Project{Status: ProjectStatus{Phase: ProjectPhaseTerminated, NodeRef: "node-a", AssignmentGeneration: 3}},
			wantBlockedBy: "has not recorded ResourcesRemoved",
		},
		{
			name:          "own obligation unresolved",
			project:       terminated(obligation("node-a", 3, false)),
			wantBlockedBy: "has not recorded ResourcesRemoved",
		},
		{
			name:          "own obligation is for another generation",
			project:       terminated(obligation("node-a", 2, true)),
			wantBlockedBy: "has not recorded ResourcesRemoved",
		},
		{
			name:          "an earlier Node still owes its cleanup",
			project:       terminated(obligation("node-b", 1, false), obligation("node-a", 3, true)),
			wantBlockedBy: "node node-b generation 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FinalizationBlocker(tt.project)
			switch {
			case tt.wantBlockedBy == "" && got != "":
				t.Fatalf("blocked by %q, want allowed", got)
			case tt.wantBlockedBy != "" && !strings.Contains(got, tt.wantBlockedBy):
				t.Fatalf("reason = %q, want it to mention %q", got, tt.wantBlockedBy)
			}
		})
	}
}

func TestFinalizationBlockerNamesEveryPendingNodeInOrder(t *testing.T) {
	p := terminated(obligation("node-c", 2, false), obligation("node-b", 1, false), obligation("node-a", 3, true))
	got := FinalizationBlocker(p)
	want := "waiting for ResourcesRemoved from node node-b generation 1, node node-c generation 2"
	if got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

func TestImmediateDeletionBlocker(t *testing.T) {
	initial := InitialProjectStatus()

	tests := []struct {
		name    string
		mutate  func(*ProjectStatus)
		allowed bool
	}{
		{name: "the initial status", mutate: func(*ProjectStatus) {}, allowed: true},
		{name: "Failed, never assigned", mutate: func(s *ProjectStatus) { s.Phase = ProjectPhaseFailed }, allowed: true},
		{name: "Scheduled", mutate: func(s *ProjectStatus) { s.Phase = ProjectPhaseScheduled }},
		{name: "Terminating", mutate: func(s *ProjectStatus) { s.Phase = ProjectPhaseTerminating }},
		{name: "history Known", mutate: func(s *ProjectStatus) { s.AssignmentHistory = AssignmentHistoryKnown }},
		{name: "history Unknown", mutate: func(s *ProjectStatus) { s.AssignmentHistory = AssignmentHistoryUnknown }},
		{name: "a nodeRef", mutate: func(s *ProjectStatus) { s.NodeRef = "node-a" }},
		{name: "a generation", mutate: func(s *ProjectStatus) { s.AssignmentGeneration = 1 }},
		{name: "an obligation", mutate: func(s *ProjectStatus) {
			s.Recovery = &ProjectRecoveryStatus{CleanupObligations: []CleanupObligation{obligation("node-a", 1, true)}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := initial
			tt.mutate(&status)
			got := ImmediateDeletionBlocker(&Project{Status: status})
			if tt.allowed && got != "" {
				t.Fatalf("blocked by %q, want allowed", got)
			}
			if !tt.allowed && got == "" {
				t.Fatal("allowed, want blocked")
			}
		})
	}
}
