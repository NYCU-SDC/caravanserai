package v1

import (
	"fmt"
	"sort"
	"strings"
)

// AwaitsFinalization reports whether a Project in status s is the server's to
// finalize: Terminated, or Terminating with no current assignment. The latter
// is a previously assigned Project deleted while Pending — no Agent holds it,
// so none will report it Terminated, and the Nodes that ran it earlier resolve
// their obligations through cleanup tasks instead.
func AwaitsFinalization(s ProjectStatus) bool {
	return s.Phase == ProjectPhaseTerminated || (s.Phase == ProjectPhaseTerminating && s.NodeRef == "")
}

// FinalizationBlocker reports why p's record may not be deleted yet, or "" when
// it may. It is the single predicate every final delete of a Project goes
// through; later authorizations (provider fencing, audited force-finalize)
// extend it here rather than add a second path.
//
// The record must outlive every runtime resource it represents, on every Node
// that may hold some. Three things must all hold:
//
//   - the lifecycle allows it (see AwaitsFinalization);
//   - the obligation list is known to be complete. For a Terminated Project
//     that means the assignment that reported it recorded ResourcesRemoved
//     for itself; for one with no current assignment, that its history is
//     Known and at least one earlier assignment is on the list. Either way an
//     empty list cannot pass on its own;
//   - every cleanup obligation in the list is resolved, so an earlier Node
//     that still holds resources keeps the record alive.
//
// The third part, the UID the decision was made for, is not a property of p:
// the store checks it against the row it locked.
func FinalizationBlocker(p *Project) string {
	s := p.Status
	switch {
	case !AwaitsFinalization(s):
		if s.Phase == ProjectPhaseTerminating {
			return fmt.Sprintf("project is Terminating on node %s, which has not reported it Terminated", s.NodeRef)
		}
		return fmt.Sprintf("project is %s, not Terminated", s.Phase)

	case s.Phase == ProjectPhaseTerminated:
		if !s.Recovery.ResourcesRemovedFor(s.NodeRef, s.AssignmentGeneration) {
			return fmt.Sprintf("node %s has not recorded ResourcesRemoved for generation %d",
				s.NodeRef, s.AssignmentGeneration)
		}

	default: // Terminating with no current assignment
		switch s.AssignmentHistory {
		case AssignmentHistoryNeverAssigned:
			// Nothing ever ran, so there is nothing to wait for; the list
			// below is empty or, if not, still has to be resolved.
		case AssignmentHistoryKnown:
			if s.Recovery == nil || len(s.Recovery.CleanupObligations) == 0 {
				return "project was assigned before, but no cleanup obligation records where"
			}
		default:
			return fmt.Sprintf("assignment history is %s, so nothing proves which Nodes may hold resources",
				s.AssignmentHistory)
		}
	}
	if pending := unresolvedObligations(s.Recovery); len(pending) > 0 {
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
