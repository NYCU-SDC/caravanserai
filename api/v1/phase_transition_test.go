package v1

import (
	"fmt"
	"testing"
)

// TestPhaseTransitionPolicy checks every (writer, from, to) combination against
// a list written out here rather than derived from the table, so a change to the
// table that this list does not also make is a failure, in either direction.
func TestPhaseTransitionPolicy(t *testing.T) {
	writers := []PhaseWriter{
		WriterAdmission, WriterDeleteAPI, WriterScheduler, WriterRescheduler,
		WriterFailedRescheduler, WriterOperatorTakeover, WriterAgent,
	}
	phases := []ProjectPhase{
		"", ProjectPhasePending, ProjectPhaseScheduled, ProjectPhaseRunning,
		ProjectPhaseFailed, ProjectPhaseTerminating, ProjectPhaseTerminated,
	}

	allowed := map[string]bool{
		"Admission: (none) -> Pending": true,

		"DeleteAPI: Pending -> Terminating":   true,
		"DeleteAPI: Scheduled -> Terminating": true,
		"DeleteAPI: Running -> Terminating":   true,
		"DeleteAPI: Failed -> Terminating":    true,

		"Scheduler: Pending -> Scheduled": true,

		"Rescheduler: Scheduled -> Pending": true,
		"Rescheduler: Running -> Pending":   true,

		"FailedRescheduler: Failed -> Pending": true,

		"OperatorTakeover: Scheduled -> Pending": true,
		"OperatorTakeover: Running -> Pending":   true,

		"Agent: Scheduled -> Running":      true,
		"Agent: Scheduled -> Failed":       true,
		"Agent: Running -> Failed":         true,
		"Agent: Terminating -> Terminated": true,
		// Reassertions the poll loop makes every tick.
		"Agent: Running -> Running":       true,
		"Agent: Failed -> Failed":         true,
		"Agent: Terminated -> Terminated": true,
	}

	seen := map[string]bool{}
	for _, w := range writers {
		for _, from := range phases {
			for _, to := range phases {
				name := string(from)
				if name == "" {
					name = "(none)"
				}
				key := fmt.Sprintf("%s: %s -> %s", w, name, to)
				seen[key] = true
				if got, want := PhaseTransitionAllowed(w, from, to), allowed[key]; got != want {
					t.Errorf("%s: allowed = %v, want %v", key, got, want)
				}
			}
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("expected transition %q names a writer or phase outside the test's lists", key)
		}
	}
}

// The transitions the guard exists for, named so a regression reads as what
// it breaks.
func TestPhaseTransitionPolicyRefusals(t *testing.T) {
	cases := []struct {
		name     string
		writer   PhaseWriter
		from, to ProjectPhase
	}{
		{"a late Running report does not undo a deletion", WriterAgent, ProjectPhaseTerminating, ProjectPhaseRunning},
		{"a late Failed report does not undo a deletion", WriterAgent, ProjectPhaseTerminating, ProjectPhaseFailed},
		{"Terminated is terminal for the Agent", WriterAgent, ProjectPhaseTerminated, ProjectPhaseRunning},
		{"the delete API does not reopen a finished deletion", WriterDeleteAPI, ProjectPhaseTerminated, ProjectPhaseTerminating},
		{"only the cleanup owner finalizes", WriterRescheduler, ProjectPhaseTerminating, ProjectPhaseTerminated},
		{"an Agent cannot assign itself", WriterAgent, ProjectPhasePending, ProjectPhaseScheduled},
		{"a takeover cannot undo a deletion", WriterOperatorTakeover, ProjectPhaseTerminating, ProjectPhasePending},
		{"an Agent cannot recover a Failed Project in place", WriterAgent, ProjectPhaseFailed, ProjectPhaseRunning},
		{"the Scheduler only assigns Pending Projects", WriterScheduler, ProjectPhaseFailed, ProjectPhaseScheduled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if PhaseTransitionAllowed(c.writer, c.from, c.to) {
				t.Errorf("%s may move %s -> %s", c.writer, c.from, c.to)
			}
		})
	}
}
