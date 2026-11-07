package adapter

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
)

var (
	_ controller.EvictionStore     = (*ProjectStoreAdapter)(nil)
	_ controller.EvictionRequester = (*ProjectStoreAdapter)(nil)
)

// RequestEviction satisfies controller.EvictionRequester. It queues a planned
// move of a Project off fromNode.
//
// It returns store.ErrConflictState when the Project is not Running on
// fromNode — only a running Project can be backed up and moved — and does
// nothing when a move is already in progress, so a trigger may ask again
// without starting a second one.
func (a *ProjectStoreAdapter) RequestEviction(ctx context.Context, name, fromNode, reason string, targetTier v1.NodeTier, avoid []string) error {
	if targetTier != "" && !targetTier.IsValid() {
		return fmt.Errorf("request eviction of %q: invalid target tier %q", name, targetTier)
	}
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		if status.Eviction != nil {
			return nil
		}
		if status.Phase != v1.ProjectPhaseRunning || status.NodeRef != fromNode {
			return fmt.Errorf("%w: project %q is %s on %q, not Running on %q",
				store.ErrConflictState, name, status.Phase, status.NodeRef, fromNode)
		}
		status.Eviction = &v1.ProjectEviction{
			Reason:      reason,
			FromNode:    fromNode,
			State:       v1.EvictionStateQueued,
			TargetTier:  targetTier,
			AvoidNodes:  slices.Clone(avoid),
			RequestedAt: now,
			StateSince:  now,
		}
		status.Conditions = v1.RemoveConditions(status.Conditions,
			v1.ConditionTypeEvictionFailed, v1.ConditionTypeEvictionBackup)
		return nil
	})
}

// ListEvictions satisfies controller.EvictionStore. It returns every Project
// with a move in progress, oldest request first.
func (a *ProjectStoreAdapter) ListEvictions(ctx context.Context) ([]*controller.EvictionSnapshot, error) {
	projects, err := a.s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []*controller.EvictionSnapshot
	for _, p := range projects {
		if p.Status.Eviction == nil {
			continue
		}
		assignment := v1.AssignmentIdentity{ProjectUID: p.ObjectMeta.UID, NodeName: p.Status.NodeRef, AssignmentGeneration: p.Status.AssignmentGeneration}
		out = append(out, &controller.EvictionSnapshot{
			Name:                    p.Name,
			UID:                     p.ObjectMeta.UID,
			Phase:                   p.Status.Phase,
			NodeRef:                 p.Status.NodeRef,
			AssignmentGeneration:    p.Status.AssignmentGeneration,
			WorkloadStopped:         p.Status.Recovery.WorkloadStoppedFor(assignment),
			Size:                    p.Spec.Size,
			FailedNodes:             slices.Clone(p.Status.FailedNodes),
			HasManagedVolumes:       p.Spec.HasManagedVolume(),
			HasPostgresDifferential: p.Spec.HasPostgresDifferential(),
			Eviction:                *p.Status.Eviction,
			Conditions:              slices.Clone(p.Status.Conditions),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Eviction.RequestedAt.Before(out[j].Eviction.RequestedAt)
	})
	return out, nil
}

// StartEvictionBackup satisfies controller.EvictionStore. It asks the agent
// on fromNode for the fresh backup, and clears any result left from an
// earlier move so the next one read is this move's.
//
// It does nothing unless the move is still Queued and the Project still
// Running on fromNode.
func (a *ProjectStoreAdapter) StartEvictionBackup(ctx context.Context, name, fromNode string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		e := status.Eviction
		if e == nil || e.State != v1.EvictionStateQueued || e.FromNode != fromNode ||
			status.Phase != v1.ProjectPhaseRunning || status.NodeRef != fromNode {
			return nil
		}
		e.State = v1.EvictionStateBackingUp
		e.StateSince = now
		status.Conditions = v1.RemoveConditions(status.Conditions, v1.ConditionTypeEvictionBackup)
		return nil
	})
}

// MoveEvictedProject satisfies controller.EvictionStore. It releases the
// Project's assignment on the source Node so the scheduler places it
// elsewhere, and reports whether it did.
//
// It goes through the same release gate as every other release: the Agent
// of the assignment must have confirmed the workload stopped (WorkloadStopped),
// which it does once it has stopped the Project for the move and, if the
// Project has Managed data, committed the fresh backup. Without that evidence
// nothing is written and the move keeps waiting. The release appends the
// source Node's cleanup obligation in the same write — and, for Managed data,
// what it owes of that data — so the scheduler places the Project elsewhere
// only once the source Node has cleaned up (ADR-0024 C2, C3).
//
// Everything is decided in one UID-bound write against the snapshot the
// controller judged: the same lifetime, still Running on the source Node
// under the same generation, with the move still in progress.
func (a *ProjectStoreAdapter) MoveEvictedProject(ctx context.Context, e *controller.EvictionSnapshot, message string) (bool, error) {
	now := time.Now().UTC()
	assignment := v1.AssignmentIdentity{ProjectUID: e.UID, NodeName: e.NodeRef, AssignmentGeneration: e.AssignmentGeneration}

	var moved bool
	err := a.s.UpdateProjectStatusForUID(ctx, e.Name, e.UID, func(status *v1.ProjectStatus) error {
		moved = false
		ev := status.Eviction
		if ev == nil || ev.State == v1.EvictionStateMoving || ev.FromNode != e.Eviction.FromNode ||
			status.NodeRef != e.NodeRef || status.AssignmentGeneration != e.AssignmentGeneration ||
			!v1.PhaseTransitionAllowed(v1.WriterEvictor, status.Phase, v1.ProjectPhasePending) {
			return nil
		}
		if !status.Recovery.WorkloadStoppedFor(assignment) {
			return nil
		}

		project, err := a.s.GetProject(ctx, e.Name)
		if err != nil {
			return err
		}

		moved = true
		ev.State = v1.EvictionStateMoving
		ev.StateSince = now
		status.Phase = v1.ProjectPhasePending
		// The generation counter and history stay: the next assignment must
		// advance past every generation this Project ever held.
		status.NodeRef = ""
		status.EnsureRecovery().AddObligation(assignment, now, v1.ReleaseByWorkloadStopped)
		if project.Spec.HasManagedVolume() {
			status.Recovery.OweManagedData(assignment)
		}
		status.Conditions = v1.RemoveConditions(status.Conditions,
			v1.ConditionTypeEvictionBackup, v1.ConditionTypeNotReadyAt, v1.ConditionTypeTerminatingAt)
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "Evicted",
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

// EndEviction satisfies controller.EvictionStore. It clears the move, and
// when failure is not nil records why it was abandoned.
func (a *ProjectStoreAdapter) EndEviction(ctx context.Context, name string, failure *controller.EvictionFailure) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		if status.Eviction == nil {
			return nil
		}
		status.Eviction = nil
		status.Conditions = v1.RemoveConditions(status.Conditions, v1.ConditionTypeEvictionBackup)
		if failure != nil {
			status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
				Type:               v1.ConditionTypeEvictionFailed,
				Status:             v1.ConditionTrue,
				Reason:             failure.Reason,
				Message:            failure.Message,
				LastTransitionTime: now,
			}, true)
		}
		return nil
	})
}
