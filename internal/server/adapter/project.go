// Package adapter bridges the broad postgres.Store CRUD interface and the
// narrow controller-specific store interfaces.  Keeping the adapters here
// (rather than in cmd/cara-server/main.go) makes them independently readable
// and avoids growing main.go with every new controller method.
package adapter

import (
	"context"
	"errors"
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

	// graceExpiredRelease lets a Project with nothing only the lost Node holds
	// leave a Node that stayed NotReady past its grace period, with no evidence
	// (ADR-0024).
	graceExpiredRelease bool
}

// NewProjectStoreAdapter returns a ProjectStoreAdapter backed by s.
func NewProjectStoreAdapter(s *pgstore.Store) *ProjectStoreAdapter {
	return &ProjectStoreAdapter{s: s}
}

// WithGraceExpiredRelease turns on the ADR-0024 transitional release for
// Projects with no Managed volume.
func (a *ProjectStoreAdapter) WithGraceExpiredRelease(enabled bool) *ProjectStoreAdapter {
	a.graceExpiredRelease = enabled
	return a
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

func (a *ProjectStoreAdapter) GetProjectPlacement(ctx context.Context, name string) (controller.ProjectPlacement, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return controller.ProjectPlacement{}, err
	}
	// A Node still owing cleanup is left out: SetProjectScheduled would refuse
	// it, and choosing it would stall a Project another Node could host.
	placement := controller.ProjectPlacement{
		UID:           project.ObjectMeta.UID,
		Phase:         project.Status.Phase,
		Size:          project.Spec.Size,
		ExcludedNodes: project.Status.PlacementExcludedNodes(),
	}
	// A Project being moved off a Node on purpose must not land back on it,
	// and must land in the tier the move asked for, if any.
	if e := project.Status.Eviction; e != nil && e.State == v1.EvictionStateMoving {
		placement.ExcludedNodes = append(placement.ExcludedNodes, e.FromNode)
		placement.RequiredTier = e.TargetTier
	}
	return placement, nil
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
func (a *ProjectStoreAdapter) SetProjectScheduled(ctx context.Context, name, uid, nodeRef string) error {
	// Fixed before the closure, which may run more than once.
	now := time.Now().UTC()
	// Pinned to the lifetime the scheduler read: a same-name Project created
	// since is refused with ErrStaleAssignment, not assigned (CARA-81 stage 3).
	return a.s.UpdateProjectStatusForUID(ctx, name, uid, func(status *v1.ProjectStatus) error {
		switch {
		case status.Phase == v1.ProjectPhaseScheduled && status.NodeRef == nodeRef:
			// A scheduler retry re-asserting the assignment it already made.
			// Not a new grant: the generation must not advance twice for one
			// grant of ownership.
			return nil
		case !v1.PhaseTransitionAllowed(v1.WriterScheduler, status.Phase, v1.ProjectPhaseScheduled):
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
		// An override permits assigning elsewhere, not back to the Node that
		// owes the cleanup.
		if slices.Contains(status.Recovery.NodesOwingCleanup(), nodeRef) {
			return fmt.Errorf("%w: node %s still owes cleanup for this project", store.ErrConflictState, nodeRef)
		}

		// One increment per real grant. Under OCC the phase read here is the
		// committed row and only the winning attempt persists, so the
		// increment is exactly-once even across retries.
		//
		// A grant of ownership is always Known history, whatever the row
		// migrated in as: the counter it now carries is a trustworthy fence.
		status.AssignmentGeneration++
		status.AssignmentHistory = v1.AssignmentHistoryKnown
		status.Conditions = v1.RemoveConditions(status.Conditions, v1.ConditionTypeSchedulingBlocked)
		// The new generation consumes the old one's no-start latch and
		// healthy window, in the same swap that commits it.
		status.Recovery.ClearForNewAssignment()
		status.Phase = v1.ProjectPhaseScheduled
		status.NodeRef = nodeRef
		// The Phase condition records the transition in the same write, so it
		// never describes the Pending the Project has just left (S9).
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "Scheduled",
			Message:            fmt.Sprintf("Assigned to node %s as generation %d", nodeRef, status.AssignmentGeneration),
			LastTransitionTime: now,
		}, true)
		return nil
	})
}

// SetSchedulingBlocked satisfies controller.SchedulerProjectStore.
func (a *ProjectStoreAdapter) SetSchedulingBlocked(ctx context.Context, name, uid, reason, message string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusForUID(ctx, name, uid, func(status *v1.ProjectStatus) error {
		if status.Phase != v1.ProjectPhasePending {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeSchedulingBlocked,
			Status:             v1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		}, false)
		return nil
	})
}

// FinalizeProject satisfies controller.TerminationProjectStore. The predicate
// is v1.FinalizationBlocker, evaluated by the store on the row it locks for the
// delete.
func (a *ProjectStoreAdapter) FinalizeProject(ctx context.Context, name, uid string) error {
	return a.s.FinalizeProject(ctx, name, uid, v1.FinalizationBlocker)
}

// SetFinalizationBlocked satisfies controller.TerminationProjectStore. It
// writes only to the lifetime the decision was made for, and only while that
// Project still awaits finalization.
func (a *ProjectStoreAdapter) SetFinalizationBlocked(ctx context.Context, name, uid, reason string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusForUID(ctx, name, uid, func(status *v1.ProjectStatus) error {
		if !v1.AwaitsFinalization(*status) {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeFinalizationBlocked,
			Status:             v1.ConditionTrue,
			Reason:             "CleanupPending",
			Message:            "Record kept: " + reason,
			LastTransitionTime: now,
			// false: re-reporting the same reason on every resync must not
			// bump the version and publish an event that enqueues this Project
			// again.
		}, false)
		return nil
	})
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
		if !v1.PhaseTransitionAllowed(v1.WriterRescheduler, status.Phase, v1.ProjectPhasePending) {
			return fmt.Errorf("%w: project %q is %s", store.ErrConflictState, p.Name, status.Phase)
		}

		// Assigned on every attempt: the closure may run more than once, and
		// only the last run's decision is the one committed.
		// Read here, after the snapshot this swap is versioned on: a spec change
		// landing later moves the version, the swap misses, and the retry reads
		// the new spec.
		project, err := a.s.GetProject(ctx, p.Name)
		if err != nil {
			return err
		}
		hasManaged := project.Spec.HasManagedVolume()

		authorization := v1.ReleaseByWorkloadStopped
		reason := "Node went NotReady and confirmed the workload stopped; project released for rescheduling"
		released = status.Recovery.WorkloadStoppedFor(assignment)
		// With Managed data and no backup the server accepted, the only copy may
		// be on the lost Node, and another Node could not start anyway. Only a
		// person can decide to give that up. PostgresDifferential backups are
		// accepted as completions rather than as a committed backup.
		//
		// A Project that turned backup off does not count as having one even
		// when an old backup exists: that backup falls further behind every
		// day, and restoring it would discard everything since without anyone
		// deciding to (ADR-0029).
		hasBackup := !project.Spec.BackupDisabled() &&
			(status.Recovery.CommittedBackupID() != "" || status.Backup.LastCompletion() != nil)
		// The grace period is no evidence the old copy stopped: a Node cut off
		// from the server keeps running it. A Project that must not run twice
		// waits for the Node or a person (ADR-0036).
		forbidsOverlap := project.Spec.ForbidsOverlap()
		if !released && a.graceExpiredRelease && !forbidsOverlap && (!hasManaged || hasBackup) {
			released = true
			authorization = v1.ReleaseByGraceExpired
			reason = "Node stayed NotReady past its grace period; project released for rescheduling"
		}
		if !released {
			message := fmt.Sprintf("Node %s is NotReady and has not confirmed generation %d stopped; "+
				"the project stays assigned to it until the node returns or the takeover is authorized",
				p.NodeRef, p.AssignmentGeneration)
			if a.graceExpiredRelease && forbidsOverlap {
				message += "; spec.overlap is Forbid, so it is not moved on the grace period"
			}
			status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
				Type:               v1.ConditionTypeTakeoverRequired,
				Status:             v1.ConditionTrue,
				Reason:             "WorkloadStopUnconfirmed",
				Message:            message,
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
		status.EnsureRecovery().AddObligation(assignment, now, authorization)
		if hasManaged {
			status.Recovery.OweManagedData(assignment)
		}
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
			Message:            reason,
			LastTransitionTime: now,
		}, transitioned)
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// The rescheduler's writes below each act on a decision taken from snapshot
// p. They go through the UID-bound status update and re-check, on the status
// the swap is versioned on and on every retry, what that decision depended on
// (CARA-81 stage 3):
//
//   - the lifetime: p's UID. A same-name Project created since is another
//     lifetime, and the write is dropped (ErrStaleAssignment is swallowed).
//   - the assignment: p's nodeRef and generation. A Project moved to another
//     Node or generation since is no longer in the outage that was judged.
//   - the phase, where the decision was phase-specific.
//   - for a clock, the outage: lastHeartbeat is the Node heartbeat the
//     decision was based on. A clock stamped at or after it was started for
//     this outage, by another pass or another server, and is not restarted;
//     a stale write landing late would otherwise move a newer outage's clock
//     back and cut its grace period short.
//
// A precondition that no longer holds ends the write without changing status.

// holdsAssignment reports whether status still carries p's assignment.
func holdsAssignment(status *v1.ProjectStatus, p *controller.ProjectSnapshot) bool {
	return status.NodeRef == p.NodeRef && status.AssignmentGeneration == p.AssignmentGeneration
}

// stampedSince reports whether conditions carry t stamped at or after since.
func stampedSince(conditions []v1.Condition, t v1.ConditionType, since time.Time) bool {
	for _, c := range conditions {
		if c.Type == t {
			return !c.LastTransitionTime.Before(since)
		}
	}
	return false
}

// ignoreOtherLifetime drops the refusal of a write pinned to a lifetime that
// is gone: for these writes that is a precondition that no longer holds.
func ignoreOtherLifetime(err error) error {
	if errors.Is(err, store.ErrStaleAssignment) {
		return nil
	}
	return err
}

// ClearRescheduleClocks satisfies controller.ReschedulerProjectStore.
func (a *ProjectStoreAdapter) ClearRescheduleClocks(ctx context.Context, p *controller.ProjectSnapshot, notAfter time.Time) error {
	return ignoreOtherLifetime(a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		if !holdsAssignment(status, p) {
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
	}))
}

// SetTerminatingAt satisfies controller.ReschedulerProjectStore. It records
// when the rescheduler first observed this Terminating project stranded on a
// NotReady node, starting the termination clock.
func (a *ProjectStoreAdapter) SetTerminatingAt(ctx context.Context, p *controller.ProjectSnapshot, at, lastHeartbeat time.Time) error {
	return ignoreOtherLifetime(a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		if !holdsAssignment(status, p) || status.Phase != v1.ProjectPhaseTerminating ||
			stampedSince(status.Conditions, v1.ConditionTypeTerminatingAt, lastHeartbeat) {
			return nil
		}
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
	}))
}

// SetNotReadyAt satisfies controller.ReschedulerProjectStore. It starts, or
// restarts, the running grace-period clock.
//
// Restarting matters as much as starting: a node can go NotReady, recover, and
// fail again, and the second failure is owed its own full grace period. That
// is why the outage check compares with lastHeartbeat rather than asking
// whether any clock exists.
func (a *ProjectStoreAdapter) SetNotReadyAt(ctx context.Context, p *controller.ProjectSnapshot, at, lastHeartbeat time.Time) error {
	return ignoreOtherLifetime(a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		if !holdsAssignment(status, p) || status.Phase != v1.ProjectPhaseRunning ||
			stampedSince(status.Conditions, v1.ConditionTypeNotReadyAt, lastHeartbeat) {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeNotReadyAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady while project was Running; running grace period clock started",
			LastTransitionTime: at,
			// true, for the same reason as SetTerminatingAt above.
		}, true)
		return nil
	}))
}

// SetTerminationBlocked satisfies controller.ReschedulerProjectStore. It marks
// a Terminating Project whose Node stayed NotReady past the termination
// timeout. The phase is left alone: the record must outlive the resources it
// represents, and nothing can confirm those are gone while the Node is away.
func (a *ProjectStoreAdapter) SetTerminationBlocked(ctx context.Context, p *controller.ProjectSnapshot, at, lastHeartbeat time.Time) error {
	return ignoreOtherLifetime(a.s.UpdateProjectStatusForUID(ctx, p.Name, p.UID, func(status *v1.ProjectStatus) error {
		if !holdsAssignment(status, p) || status.Phase != v1.ProjectPhaseTerminating ||
			stampedSince(status.Conditions, v1.ConditionTypeTerminationBlocked, lastHeartbeat) {
			return nil
		}
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:   v1.ConditionTypeTerminationBlocked,
			Status: v1.ConditionTrue,
			Reason: "NodeNotReady",
			Message: fmt.Sprintf("Node %s has been NotReady past the termination timeout; "+
				"the project is kept until the node returns and confirms its resources are removed", p.NodeRef),
			LastTransitionTime: at,
			// true, for the same reason as SetTerminatingAt: the timestamp is
			// what says which outage this mark belongs to.
		}, true)
		return nil
	}))
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
		Evicting:             p.Status.Eviction != nil,
		ExcludedNodes:        p.Status.PlacementExcludedNodes(),
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
		if !v1.PhaseTransitionAllowed(v1.WriterFailedRescheduler, status.Phase, v1.ProjectPhasePending) ||
			status.NodeRef != p.NodeRef || status.AssignmentGeneration != p.AssignmentGeneration {
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

		project, err := a.s.GetProject(ctx, p.Name)
		if err != nil {
			return err
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
		if project.Spec.HasManagedVolume() {
			status.Recovery.OweManagedData(assignment)
		}
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
