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
func (a *ProjectStoreAdapter) RequestEviction(ctx context.Context, name, fromNode, reason string) error {
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
		out = append(out, &controller.EvictionSnapshot{
			Name:              p.Name,
			Phase:             p.Status.Phase,
			NodeRef:           p.Status.NodeRef,
			Size:              p.Spec.Size,
			FailedNodes:       slices.Clone(p.Status.FailedNodes),
			HasManagedVolumes: hasManagedVolume(p.Spec.Volumes),
			Eviction:          *p.Status.Eviction,
			Conditions:        slices.Clone(p.Status.Conditions),
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

// MoveEvictedProject satisfies controller.EvictionStore. It returns the
// Project to Pending off fromNode so the scheduler places it elsewhere.
//
// It does nothing unless a move is in progress and the Project is still
// Running on fromNode. As in SetProjectPending, clearing nodeRef revokes the
// assignment but leaves the generation counter and assignment history alone.
func (a *ProjectStoreAdapter) MoveEvictedProject(ctx context.Context, name, fromNode, message string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		e := status.Eviction
		if e == nil || e.State == v1.EvictionStateMoving || e.FromNode != fromNode ||
			status.Phase != v1.ProjectPhaseRunning || status.NodeRef != fromNode {
			return nil
		}
		e.State = v1.EvictionStateMoving
		e.StateSince = now
		status.Phase = v1.ProjectPhasePending
		status.NodeRef = ""
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

// hasManagedVolume reports whether volumes holds any Managed volume — data a
// move has to carry to the new Node.
func hasManagedVolume(volumes []v1.VolumeDef) bool {
	for _, v := range volumes {
		if v.Type == v1.VolumeTypeManaged {
			return true
		}
	}
	return false
}
