package adapter

import (
	"context"
	"slices"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
)

// DrainStoreAdapter wraps *pgstore.Store and satisfies controller.DrainStore.
type DrainStoreAdapter struct {
	*ProjectStoreAdapter
	s *pgstore.Store
}

// NewDrainStoreAdapter returns a DrainStoreAdapter backed by s.
func NewDrainStoreAdapter(s *pgstore.Store) *DrainStoreAdapter {
	return &DrainStoreAdapter{ProjectStoreAdapter: NewProjectStoreAdapter(s), s: s}
}

var _ controller.DrainStore = (*DrainStoreAdapter)(nil)

// ListNodeDrains satisfies controller.DrainStore. It returns every Node that
// is being drained, or still carries the record of a drain that was called
// off.
func (a *DrainStoreAdapter) ListNodeDrains(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range nodes {
		if n.Spec.Drain || n.Status.Drain != nil {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// GetNodeDrain satisfies controller.DrainStore.
func (a *DrainStoreAdapter) GetNodeDrain(ctx context.Context, name string) (controller.NodeDrainSnapshot, error) {
	n, err := a.s.GetNode(ctx, name)
	if err != nil {
		return controller.NodeDrainSnapshot{}, err
	}
	return controller.NodeDrainSnapshot{Name: n.Name, Drain: n.Spec.Drain, Status: n.Status.Drain}, nil
}

// ListProjectsOnNode satisfies controller.DrainStore. It returns every
// Project assigned to the Node, whatever its phase.
func (a *DrainStoreAdapter) ListProjectsOnNode(ctx context.Context, node string) ([]controller.DrainProjectSnapshot, error) {
	projects, err := a.s.ListProjectsByNodeRef(ctx, node, []v1.ProjectPhase{
		v1.ProjectPhasePending, v1.ProjectPhaseScheduled, v1.ProjectPhaseRunning,
		v1.ProjectPhaseFailed, v1.ProjectPhaseTerminating, v1.ProjectPhaseTerminated,
	})
	if err != nil {
		return nil, err
	}
	out := make([]controller.DrainProjectSnapshot, 0, len(projects))
	for _, p := range projects {
		snap := controller.DrainProjectSnapshot{
			Name:     p.Name,
			Phase:    p.Status.Phase,
			Eviction: p.Status.Eviction,
		}
		for _, c := range p.Status.Conditions {
			if c.Type == v1.ConditionTypeEvictionFailed && c.Status == v1.ConditionTrue {
				failed := c
				snap.EvictionFailed = &failed
			}
		}
		out = append(out, snap)
	}
	return out, nil
}

// ListProjectsOwedCleanup satisfies controller.DrainStore. It names the
// Projects the Node still owes cleanup for: their old containers or local
// Managed data are still there.
func (a *DrainStoreAdapter) ListProjectsOwedCleanup(ctx context.Context, node string) ([]string, error) {
	projects, err := a.s.ListProjectsOwingCleanup(ctx, node)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range projects {
		if slices.Contains(p.Status.Recovery.NodesOwingCleanup(), node) {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

// SetNodeDrainStatus satisfies controller.DrainStore. A nil status clears it.
//
// It changes only status.drain and the NodeDrainIncomplete condition, on the
// status as it is when written: the heartbeat and the health controller write
// the same status, and writing back a copy read earlier would undo them — a
// Node just marked NotReady would read Ready again.
func (a *DrainStoreAdapter) SetNodeDrainStatus(ctx context.Context, name string, drain *v1.NodeDrainStatus) error {
	return a.s.UpdateNodeStatusWithRetry(ctx, name, func(status *v1.NodeStatus) error {
		status.Drain = drain
		status.Conditions = v1.RemoveConditions(status.Conditions, v1.ConditionTypeNodeDrainIncomplete)
		if drain != nil && drain.Phase == v1.NodeDrainIncomplete {
			status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
				Type:               v1.ConditionTypeNodeDrainIncomplete,
				Status:             v1.ConditionTrue,
				Reason:             "ProjectsCannotLeave",
				Message:            strings.Join(drain.Blocking, "; "),
				LastTransitionTime: drain.Since,
			}, false)
		}
		return nil
	})
}

// CancelEviction satisfies controller.DrainStore. It calls off a planned move
// of the Project off fromNode for reason, but only while that is still safe:
// the move is Queued, or BackingUp with no stop evidence yet. Once the Agent
// has confirmed the workload stopped, or the Project has left, the move is
// past recall and is left to finish. It reports whether it cancelled.
func (a *DrainStoreAdapter) CancelEviction(ctx context.Context, name, fromNode, reason string) (bool, error) {
	var cancelled bool
	err := a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		cancelled = false
		e := status.Eviction
		if e == nil || e.Reason != reason || e.FromNode != fromNode || status.NodeRef != fromNode {
			return nil
		}
		switch e.State {
		case v1.EvictionStateQueued:
		case v1.EvictionStateBackingUp:
			p, err := a.s.GetProject(ctx, name)
			if err != nil {
				return err
			}
			current := v1.AssignmentIdentity{ProjectUID: p.UID, NodeName: status.NodeRef, AssignmentGeneration: status.AssignmentGeneration}
			if status.Recovery.WorkloadStoppedFor(current) {
				return nil
			}
		default:
			return nil
		}
		cancelled = true
		status.Eviction = nil
		status.Conditions = v1.RemoveConditions(status.Conditions, v1.ConditionTypeEvictionBackup)
		return nil
	})
	return cancelled, err
}
