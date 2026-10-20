// Package adapter bridges the broad postgres.Store CRUD interface and the
// narrow controller-specific store interfaces.  Keeping the adapters here
// (rather than in cmd/cara-server/main.go) makes them independently readable
// and avoids growing main.go with every new controller method.
package adapter

import (
	"context"
	"fmt"
	"slices"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
)

// Compile-time interface satisfaction checks.
var (
	_ controller.SchedulerProjectStore   = (*ProjectStoreAdapter)(nil)
	_ controller.TerminationProjectStore = (*ProjectStoreAdapter)(nil)
	_ controller.ReschedulerProjectStore = (*ProjectStoreAdapter)(nil)
)

// ProjectStoreAdapter wraps *pgstore.Store and satisfies
// controller.SchedulerProjectStore, controller.TerminationProjectStore, and
// controller.ReschedulerProjectStore.
type ProjectStoreAdapter struct {
	s *pgstore.Store
}

// NewProjectStoreAdapter returns a ProjectStoreAdapter backed by s.
func NewProjectStoreAdapter(s *pgstore.Store) *ProjectStoreAdapter {
	return &ProjectStoreAdapter{s: s}
}

func (a *ProjectStoreAdapter) ListProjectNamesByPhase(ctx context.Context, phase v1.ProjectPhase) ([]string, error) {
	projects, err := a.s.ListProjectsByPhase(ctx, phase)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names, nil
}

func (a *ProjectStoreAdapter) GetProjectPhase(ctx context.Context, name string) (v1.ProjectPhase, string, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return "", "", err
	}
	return project.Status.Phase, project.Status.NodeRef, nil
}

func (a *ProjectStoreAdapter) GetProjectPlacement(ctx context.Context, name string) (controller.ProjectPlacement, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return controller.ProjectPlacement{}, err
	}
	return controller.ProjectPlacement{
		Size:          project.Spec.Size,
		ExcludedNodes: slices.Clone(project.Status.FailedNodes),
	}, nil
}

// SetProjectScheduled satisfies controller.SchedulerProjectStore.
//
// Only a Pending Project may receive a new assignment, and only a Scheduled
// Project already on nodeRef may have it re-asserted (a no-op). Everything
// else is refused with ErrConflictState, writing nothing. The phase is checked
// on the snapshot the swap is versioned on, because the scheduler decided to
// act on an earlier read: in between, the Project can be deleted into
// Terminating — and writing Scheduled then would undo the deletion — or be
// assigned elsewhere, and silently moving a Scheduled Project to another Node
// would change its Node without a new generation, a release or a cleanup
// obligation.
func (a *ProjectStoreAdapter) SetProjectScheduled(ctx context.Context, name, nodeRef string) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		switch {
		case status.Phase == v1.ProjectPhaseScheduled && status.NodeRef == nodeRef:
			// A scheduler retry re-asserting the assignment it already made.
			// Not a new grant: the generation must not advance twice for one
			// grant of ownership.
			return nil
		case status.Phase != v1.ProjectPhasePending:
			return fmt.Errorf("%w: project %q is %s on node %q; only a Pending project can be assigned",
				store.ErrConflictState, name, status.Phase, status.NodeRef)
		}

		// Refused for every Node, the previous one included: a Node still
		// owing cleanup for an older generation must not be handed a new one
		// by an ordinary assignment, or its cleanup would remove the new
		// generation's resources. Same-Node takeover goes through its own
		// handoff mutation instead. Checked on the same snapshot as the
		// phase, so an obligation appended after the read turns the swap into
		// a retry that sees it.
		if o := status.Recovery.BlockingObligation(); o != nil {
			return fmt.Errorf("%w: node %s has not removed the resources of generation %d (released by %s)",
				store.ErrConflictState, o.Assignment.NodeName, o.Assignment.AssignmentGeneration, o.ReleaseAuthorization)
		}

		// One increment per real grant. Under OCC the phase read here is the
		// committed row and only the winning attempt persists, so the
		// increment is exactly-once even across retries.
		//
		// A grant of ownership is always Known history, whatever the row
		// migrated in as: the counter it now carries is a trustworthy fence.
		status.AssignmentGeneration++
		status.AssignmentHistory = v1.AssignmentHistoryKnown
		// The new generation consumes the old one's no-start latch and
		// healthy window, in the same swap that commits it.
		status.Recovery.ClearForNewAssignment()
		status.Phase = v1.ProjectPhaseScheduled
		status.NodeRef = nodeRef
		return nil
	})
}

func (a *ProjectStoreAdapter) SetProjectPhase(ctx context.Context, name string, phase v1.ProjectPhase, reason, message string) error {
	// Fixed before the closure, not inside it: the closure may run more than
	// once, and this records when the transition was observed rather than when
	// a retry happened to land.
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		transitioned := status.Phase != phase
		status.Phase = phase
		if phase == v1.ProjectPhaseFailed {
			status.Recovery.ClearHealthyWindow()
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		}, transitioned)
		return nil
	})
}

func (a *ProjectStoreAdapter) DeleteProject(ctx context.Context, name string) error {
	return a.s.DeleteProject(ctx, name)
}

// ListProjectsByNodeRef satisfies controller.ReschedulerProjectStore.
// It converts api/v1 Projects into controller.ProjectSnapshot values.
func (a *ProjectStoreAdapter) ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*controller.ProjectSnapshot, error) {
	projects, err := a.s.ListProjectsByNodeRef(ctx, nodeRef, phases)
	if err != nil {
		return nil, err
	}
	snapshots := make([]*controller.ProjectSnapshot, len(projects))
	for i, p := range projects {
		snapshots[i] = snapshotOf(p)
	}
	return snapshots, nil
}

// releasablePhases are the phases the rescheduler releases from. Terminating
// is handled by its own timeout and is never handed to another Node.
var releasablePhases = map[v1.ProjectPhase]bool{
	v1.ProjectPhaseScheduled: true,
	v1.ProjectPhaseRunning:   true,
}

// ReleaseAssignment satisfies controller.ReschedulerProjectStore.
//
// The gate and the release are one compare-and-swap pinned to the snapshot's
// UID: the WorkloadStopped evidence, the assignment and the phase are all
// checked on the status the swap is versioned on. Releasing first and checking
// afterwards would leave a window in which the Project is Pending, schedulable
// elsewhere, while its old containers may still run.
func (a *ProjectStoreAdapter) ReleaseAssignment(ctx context.Context, p *controller.ProjectSnapshot) (bool, error) {
	now := time.Now().UTC()
	assignment := v1.AssignmentIdentity{ProjectUID: p.UID, NodeName: p.NodeRef, AssignmentGeneration: p.AssignmentGeneration}

	var released bool
	err := a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		if status.NodeRef != p.NodeRef || status.AssignmentGeneration != p.AssignmentGeneration {
			return fmt.Errorf("%w: project %q no longer holds node %s generation %d",
				store.ErrStaleAssignment, p.Name, p.NodeRef, p.AssignmentGeneration)
		}
		if !releasablePhases[status.Phase] {
			return fmt.Errorf("%w: project %q is %s", store.ErrConflictState, p.Name, status.Phase)
		}

		// Assigned on every attempt: the closure may run more than once, and
		// only the last run's decision is the one committed.
		released = status.Recovery.WorkloadStoppedFor(assignment)
		if !released {
			status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
				Type:   v1.ConditionTypeTakeoverRequired,
				Status: v1.ConditionTrue,
				Reason: "WorkloadStopUnconfirmed",
				Message: fmt.Sprintf("Node %s is NotReady and has not confirmed generation %d stopped; "+
					"the project stays assigned to it until the node returns or the takeover is authorized",
					p.NodeRef, p.AssignmentGeneration),
				LastTransitionTime: now,
			}, false)
			return nil
		}

		transitioned := status.Phase != v1.ProjectPhasePending
		status.Phase = v1.ProjectPhasePending
		// Clearing nodeRef revokes the current assignment but must not reset the
		// generation counter: the next assignment has to advance past every
		// generation this Project ever held, or a stale report from an earlier
		// owner could match a recycled number. AssignmentHistory likewise stays
		// as-is (it was assigned, so it is Known) — releasing ownership never
		// makes a Project look never-assigned.
		status.NodeRef = ""
		// The released Node still holds the stopped containers. Recording
		// that in the same write is what makes the obligation list complete:
		// no reader can see the assignment gone without its debt recorded.
		status.EnsureRecovery().AddObligation(assignment, now, v1.ReleaseByWorkloadStopped)
		// The incident is over for this Project: it is leaving the node that
		// went NotReady. The outage conditions are scoped to that outage, so
		// they go with it — carried to the next node, a clock would be read as
		// the start of that node's first failure, which by then is already
		// past every timeout.
		status.Conditions = v1.RemoveConditions(status.Conditions,
			v1.ConditionTypeNotReadyAt, v1.ConditionTypeTerminatingAt, v1.ConditionTypeTakeoverRequired)
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady and confirmed the workload stopped; project released for rescheduling",
			LastTransitionTime: now,
		}, transitioned)
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// ClearRescheduleClocks satisfies controller.ReschedulerProjectStore.
// Removes the outage conditions (NotReadyAt, TerminatingAt, TakeoverRequired,
// TerminationBlocked)
// from a Project still assigned to nodeRef, ending the reschedule clocks
// without touching the phase.
//
// This is the other way an incident ends: the node came back before either
// clock expired, so the Project never moved and its phase never changed. The
// clocks no longer govern anything — handleRunning and handleTerminating both
// ignore a clock older than the node's last heartbeat — but they are still in
// Project status, reporting a failure that is over to anyone who reads it.
//
// Two guards, because this write races the next outage. The cleanup decides to
// run while the node is Ready; between that decision and this write the node
// can fail again and a fresh clock be written, and deleting that one would
// hand the Project an extra full grace period:
//
//   - notAfter is the heartbeat that proved the node healthy. A clock stamped
//     later than it belongs to an outage that began after this cleanup was
//     decided on, and is left alone.
//   - nodeRef must still be the Project's, so a Project reassigned in the
//     meantime is not written by the old node's cleanup at all.
//
// Writing unconditionally is otherwise fine: UpdateProjectStatusWithRetry is a
// no-op when the status is unchanged, and the caller only asks for Projects
// that carry a clock.
func (a *ProjectStoreAdapter) ClearRescheduleClocks(
	ctx context.Context,
	name, nodeRef string,
	notAfter time.Time,
) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		if status.NodeRef != nodeRef {
			return nil
		}

		kept := make([]v1.Condition, 0, len(status.Conditions))
		for _, c := range status.Conditions {
			if controller.IsOutageCondition(c.Type) && !c.LastTransitionTime.After(notAfter) {
				continue
			}
			kept = append(kept, c)
		}
		status.Conditions = kept
		return nil
	})
}

// SetTerminatingAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the TerminatingAt condition to record the time at which
// the rescheduler first observed this project as stranded on a NotReady node.
func (a *ProjectStoreAdapter) SetTerminatingAt(ctx context.Context, name string, at time.Time) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeTerminatingAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady while project was Terminating; termination timeout clock started",
			LastTransitionTime: at,
			// true, not false: every field but the timestamp is a fixed
			// string, so an unchanged-content check would keep the previous
			// incident's start time and the clock would never restart.
		}, true)
		return nil
	})
}

// SetNotReadyAt satisfies controller.ReschedulerProjectStore.
// Starts, or restarts, the running grace-period clock at the given time.
//
// Restarting matters as much as starting: a node can go NotReady, recover, and
// fail again, and the second failure is owed its own full grace period. The
// caller only reaches here when it has decided a clock should begin, so this
// always writes the timestamp it was given.
func (a *ProjectStoreAdapter) SetNotReadyAt(ctx context.Context, name string, at time.Time) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeNotReadyAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady while project was Running; running grace period clock started",
			LastTransitionTime: at,
			// true, for the same reason as SetTerminatingAt above.
		}, true)
		return nil
	})
}

// SetTerminationBlocked satisfies controller.ReschedulerProjectStore.
// Marks a Terminating Project whose Node stayed NotReady past the termination
// timeout. The phase is left alone: the record must outlive the resources it
// represents, and nothing can confirm those are gone while the Node is away.
//
// A Project that has left Terminating since the controller read it is not
// written, so the mark never lands on a phase it does not describe.
func (a *ProjectStoreAdapter) SetTerminationBlocked(ctx context.Context, name, nodeRef string, at time.Time) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		if status.Phase != v1.ProjectPhaseTerminating {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:   v1.ConditionTypeTerminationBlocked,
			Status: v1.ConditionTrue,
			Reason: "NodeNotReady",
			Message: fmt.Sprintf("Node %s has been NotReady past the termination timeout; "+
				"the project is kept until the node returns and confirms its resources are removed", nodeRef),
			LastTransitionTime: at,
			// true, for the same reason as SetTerminatingAt: the timestamp is
			// what says which outage this mark belongs to.
		}, true)
		return nil
	})
}

// snapshotOf converts an api/v1 Project into the view the controllers read.
func snapshotOf(p *v1.Project) *controller.ProjectSnapshot {
	conditions := make([]controller.ConditionSnapshot, len(p.Status.Conditions))
	for i, c := range p.Status.Conditions {
		conditions[i] = controller.ConditionSnapshot{
			Type:               c.Type,
			Reason:             c.Reason,
			LastTransitionTime: c.LastTransitionTime,
		}
	}
	return &controller.ProjectSnapshot{
		Name:                 p.Name,
		UID:                  p.ObjectMeta.UID,
		Phase:                p.Status.Phase,
		NodeRef:              p.Status.NodeRef,
		AssignmentGeneration: p.Status.AssignmentGeneration,
		Size:                 p.Spec.Size,
		FailedNodes:          slices.Clone(p.Status.FailedNodes),
		Conditions:           conditions,
	}
}

// GetProjectSnapshot satisfies controller.FailedProjectStore.
func (a *ProjectStoreAdapter) GetProjectSnapshot(ctx context.Context, name string) (*controller.ProjectSnapshot, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return nil, err
	}
	return snapshotOf(project), nil
}

// MoveFailedProject satisfies controller.FailedProjectStore. It releases a
// Failed Project's assignment so the scheduler places it again, and records the
// Node it leaves in FailedNodes so the scheduler does not choose it again.
//
// Failed is not stopped. A Project fails when its containers keep exiting, but
// a crash-looping container may be running at this moment, and the Node is up.
// So the move goes through the same gate as every other release: the assigned
// Agent must have confirmed that this generation's containers are stopped
// (WorkloadStopped). Without that the Project stays Failed on its Node with
// TakeoverRequired, and nothing about its moves changes — FailedNodes is not
// extended, so no move is spent on one that did not happen.
//
// Everything is decided in one UID-bound write against the snapshot the
// controller judged: the Project must still be the same lifetime, Failed, on the
// same Node and generation. A release appends the cleanup obligation in that
// same write, so no reader can see the assignment gone without its debt.
//
// It reports whether the Project was moved. Calling it again after a move is a
// no-op: the Project is no longer Failed on that Node.
func (a *ProjectStoreAdapter) MoveFailedProject(ctx context.Context, p *controller.ProjectSnapshot, message string) (bool, error) {
	now := time.Now().UTC()
	assignment := v1.AssignmentIdentity{ProjectUID: p.UID, NodeName: p.NodeRef, AssignmentGeneration: p.AssignmentGeneration}

	var moved bool
	err := a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		moved = false
		if status.Phase != v1.ProjectPhaseFailed || status.NodeRef != p.NodeRef ||
			status.AssignmentGeneration != p.AssignmentGeneration {
			// Applied again, deleted, moved, or reassigned since it was judged.
			return nil
		}

		if !status.Recovery.WorkloadStoppedFor(assignment) {
			status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
				Type:   v1.ConditionTypeTakeoverRequired,
				Status: v1.ConditionTrue,
				Reason: "WorkloadStopUnconfirmed",
				Message: fmt.Sprintf("Failed on node %s, which has not confirmed generation %d stopped; "+
					"the project stays assigned to it until the stop is confirmed or the takeover is authorized",
					p.NodeRef, p.AssignmentGeneration),
				LastTransitionTime: now,
			}, false)
			return nil
		}

		moved = true
		if !slices.Contains(status.FailedNodes, p.NodeRef) {
			status.FailedNodes = append(status.FailedNodes, p.NodeRef)
		}
		status.Phase = v1.ProjectPhasePending
		// The generation counter and history stay: the next assignment must
		// advance past every generation this Project ever held.
		status.NodeRef = ""
		status.EnsureRecovery().AddObligation(assignment, now, v1.ReleaseByWorkloadStopped)
		status.Conditions = v1.RemoveConditions(status.Conditions,
			v1.ConditionTypeNotReadyAt, v1.ConditionTypeTerminatingAt, v1.ConditionTypeTakeoverRequired)
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "RescheduledAfterFailure",
			Message:            message,
			LastTransitionTime: now,
		}, true)
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}

// ClearFailedNodes satisfies controller.FailedProjectStore.
func (a *ProjectStoreAdapter) ClearFailedNodes(ctx context.Context, name string) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.FailedNodes = nil
		return nil
	})
}

// SetRescheduleExhausted satisfies controller.FailedProjectStore. It marks a
// Failed Project as out of moves; it does nothing for a Project that is no
// longer Failed.
func (a *ProjectStoreAdapter) SetRescheduleExhausted(ctx context.Context, name, message string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		if status.Phase != v1.ProjectPhaseFailed {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeRescheduleExhausted,
			Status:             v1.ConditionTrue,
			Reason:             "RetriesExhausted",
			Message:            message,
			LastTransitionTime: now,
		}, false)
		return nil
	})
}
