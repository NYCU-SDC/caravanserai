package v1

import (
	"fmt"
	"sort"
	"strings"
)

// FinalizationBlocker reports why p's record may not be deleted yet, or "" when
// it may. It is the single predicate every final delete of a Project goes
// through; later authorizations (provider fencing, audited force-finalize)
// extend it here rather than add a second path.
//
// The record must outlive every runtime resource it represents, on every Node
// that may hold some. Three things must all hold:
//
//   - the lifecycle allows it: the Project is Terminated;
//   - the assignment that reported Terminated recorded ResourcesRemoved for
//     itself, so an empty obligation list cannot pass on its own;
//   - every cleanup obligation in the list is resolved, so an earlier Node
//     that still holds resources keeps the record alive.
//
// The third part, the UID the decision was made for, is not a property of p:
// the store checks it against the row it locked.
func FinalizationBlocker(p *Project) string {
	if p.Status.Phase != ProjectPhaseTerminated {
		return fmt.Sprintf("project is %s, not Terminated", p.Status.Phase)
	}
	if !p.Status.Recovery.ResourcesRemovedFor(p.Status.NodeRef, p.Status.AssignmentGeneration) {
		return fmt.Sprintf("node %s has not recorded ResourcesRemoved for generation %d",
			p.Status.NodeRef, p.Status.AssignmentGeneration)
	}
	if pending := unresolvedObligations(p.Status.Recovery); len(pending) > 0 {
		return "waiting for ResourcesRemoved from " + strings.Join(pending, ", ")
	}
	return ""
}

// ImmediateDeletionBlocker reports why p may not be deleted directly, without
// going through Terminating, or "" when it may.
//
// Direct deletion is safe only for a Project no Agent has ever held: then there
// is nothing on any Node for the record to outlive. NeverAssigned is what says
// so, and the rest is checked alongside it rather than trusted from it — a
// first assignment that commits after the caller read the Project leaves a
// nodeRef and a generation behind even if a history were somehow stale.
func ImmediateDeletionBlocker(p *Project) string {
	s := p.Status
	switch {
	case s.Phase != ProjectPhasePending && s.Phase != ProjectPhaseFailed:
		return fmt.Sprintf("project is %s", s.Phase)
	case s.AssignmentHistory != AssignmentHistoryNeverAssigned:
		return fmt.Sprintf("assignment history is %s", s.AssignmentHistory)
	case s.NodeRef != "":
		return fmt.Sprintf("project is assigned to node %s", s.NodeRef)
	case s.AssignmentGeneration != 0:
		return fmt.Sprintf("project is at assignment generation %d", s.AssignmentGeneration)
	case s.Recovery != nil && len(s.Recovery.CleanupObligations) > 0:
		return "project has cleanup obligations"
	}
	return ""
}

// unresolvedObligations names, in a stable order, every assignment in r whose
// cleanup has not been confirmed.
func unresolvedObligations(r *ProjectRecoveryStatus) []string {
	if r == nil {
		return nil
	}
	var pending []string
	for _, o := range r.CleanupObligations {
		if !o.Resolved() {
			pending = append(pending, fmt.Sprintf("node %s generation %d",
				o.Assignment.NodeName, o.Assignment.AssignmentGeneration))
		}
	}
	sort.Strings(pending)
	return pending
}
