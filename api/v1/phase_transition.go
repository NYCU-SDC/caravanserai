package v1

// PhaseWriter names a logical writer of a Project's phase. Each lifecycle
// transition belongs to exactly one of them (ADR-0016): a phase change is legal
// only when the writer asking for it is the one the transition belongs to.
type PhaseWriter string

const (
	// WriterAdmission is the create API, the only writer of a new Project.
	WriterAdmission PhaseWriter = "Admission"
	// WriterDeleteAPI is the delete API moving a Project into Terminating.
	WriterDeleteAPI PhaseWriter = "DeleteAPI"
	// WriterScheduler commits a new assignment.
	WriterScheduler PhaseWriter = "Scheduler"
	// WriterRescheduler releases the assignment of a Project whose Node is
	// NotReady.
	WriterRescheduler PhaseWriter = "Rescheduler"
	// WriterFailedRescheduler releases the assignment of a Failed Project so
	// it can be moved to another Node. ADR-0016 calls it the Recovery
	// Controller.
	WriterFailedRescheduler PhaseWriter = "FailedRescheduler"
	// WriterOperatorTakeover releases the assignment of a Project whose Node
	// will not report back, on an operator's audited override.
	WriterOperatorTakeover PhaseWriter = "OperatorTakeover"
	// WriterAgent is a fenced Agent report: the assigned Agent reporting its
	// workload, or the cleanup owner reporting a finished teardown.
	WriterAgent PhaseWriter = "Agent"
)

// phaseTransition is one edge of the lifecycle.
type phaseTransition struct {
	from, to ProjectPhase
}

// phaseTransitions lists every transition each writer may make. Anything not
// listed is refused, so a new writer or a new phase starts with no permissions
// rather than all of them.
//
// Preconditions beyond the phase — a matching fence, stop evidence, a resolved
// cleanup obligation, no blocking obligation — are checked by the mutation that
// applies the transition, on the same snapshot. This table only says who may
// move a Project from where to where.
//
// There is deliberately no time-based Terminating → Terminated edge: only the
// cleanup owner can say its resources are gone, so only it may finalize.
var phaseTransitions = map[PhaseWriter]map[phaseTransition]bool{
	WriterAdmission: {
		{"", ProjectPhasePending}: true,
	},
	WriterDeleteAPI: {
		// Pending reaches here only with assignment history; a never-assigned
		// Pending Project is deleted directly instead.
		{ProjectPhasePending, ProjectPhaseTerminating}:   true,
		{ProjectPhaseScheduled, ProjectPhaseTerminating}: true,
		{ProjectPhaseRunning, ProjectPhaseTerminating}:   true,
		{ProjectPhaseFailed, ProjectPhaseTerminating}:    true,
	},
	WriterScheduler: {
		{ProjectPhasePending, ProjectPhaseScheduled}: true,
	},
	WriterRescheduler: {
		{ProjectPhaseScheduled, ProjectPhasePending}: true,
		{ProjectPhaseRunning, ProjectPhasePending}:   true,
	},
	WriterFailedRescheduler: {
		{ProjectPhaseFailed, ProjectPhasePending}: true,
	},
	WriterOperatorTakeover: {
		{ProjectPhaseScheduled, ProjectPhasePending}: true,
		{ProjectPhaseRunning, ProjectPhasePending}:   true,
	},
	WriterAgent: {
		{ProjectPhaseScheduled, ProjectPhaseRunning}:      true,
		{ProjectPhaseScheduled, ProjectPhaseFailed}:       true,
		{ProjectPhaseRunning, ProjectPhaseFailed}:         true,
		{ProjectPhaseTerminating, ProjectPhaseTerminated}: true,
	},
}

// agentReassertable are the phases an Agent may report again unchanged. Its
// poll loop re-reports what it observes, and refusing an identical report would
// turn a healthy Project's every tick into an error. A report that merely
// repeats the phase still has to pass the fence, so it cannot be used to touch
// a Project the Agent no longer owns.
var agentReassertable = map[ProjectPhase]bool{
	ProjectPhaseRunning:    true,
	ProjectPhaseFailed:     true,
	ProjectPhaseTerminated: true,
}

// PhaseTransitionAllowed reports whether writer may move a Project from phase
// from to phase to. An unchanged phase is a transition too: it is allowed only
// where the writer legitimately repeats itself, so a writer cannot use a no-op
// report on a phase that is not its own to keep.
func PhaseTransitionAllowed(writer PhaseWriter, from, to ProjectPhase) bool {
	if from == to {
		return writer == WriterAgent && agentReassertable[to]
	}
	return phaseTransitions[writer][phaseTransition{from, to}]
}
