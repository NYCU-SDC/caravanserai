package adapter

import (
	"context"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
)

// Compile-time interface satisfaction checks.
var (
	_ controller.NodeStore            = (*NodeStoreAdapter)(nil)
	_ controller.ReschedulerNodeStore = (*NodeStoreAdapter)(nil)
	_ controller.SchedulerNodeStore   = (*NodeReadyAdapter)(nil)
)

// NodeStoreAdapter wraps *pgstore.Store and satisfies controller.NodeStore and
// controller.ReschedulerNodeStore.
type NodeStoreAdapter struct {
	s *pgstore.Store
}

// NewNodeStoreAdapter returns a NodeStoreAdapter backed by s.
func NewNodeStoreAdapter(s *pgstore.Store) *NodeStoreAdapter {
	return &NodeStoreAdapter{s: s}
}

func (a *NodeStoreAdapter) ListNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names, nil
}

func (a *NodeStoreAdapter) GetNodeStatus(ctx context.Context, name string) (controller.NodeStatusSnapshot, error) {
	node, err := a.s.GetNode(ctx, name)
	if err != nil {
		return controller.NodeStatusSnapshot{}, err
	}
	return controller.NodeStatusSnapshot{
		LastHeartbeat: node.Status.LastHeartbeat,
		State:         node.Status.State,
	}, nil
}

// SetNodeState satisfies controller.NodeStore.
//
// It changes only the state and the Ready condition, and only if no heartbeat
// has arrived since the one the decision was made on: a NotReady decided on a
// stale read must not land on a Node that has just been heard from. The write
// is versioned, so a heartbeat or drain status written meanwhile is kept.
func (a *NodeStoreAdapter) SetNodeState(ctx context.Context, name string, state v1.NodeState, reason, message string, observedHeartbeat time.Time) error {
	now := time.Now().UTC()
	return a.s.UpdateNodeStatusWithRetry(ctx, name, func(status *v1.NodeStatus) error {
		if status.LastHeartbeat.After(observedHeartbeat) {
			return nil
		}
		status.State = state
		condStatus := v1.ConditionTrue
		if state != v1.NodeStateReady {
			condStatus = v1.ConditionFalse
		}
		ready := v1.Condition{
			Type:               v1.ConditionTypeReady,
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		}
		for i, c := range status.Conditions {
			if c.Type == v1.ConditionTypeReady {
				status.Conditions[i] = ready
				return nil
			}
		}
		status.Conditions = append(status.Conditions, ready)
		return nil
	})
}

// ListNotReadyNodeNames satisfies controller.ReschedulerNodeStore.
func (a *NodeStoreAdapter) ListNotReadyNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range nodes {
		if n.Status.State == v1.NodeStateNotReady {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// NodeReadyAdapter wraps *pgstore.Store and satisfies controller.SchedulerNodeStore.
type NodeReadyAdapter struct {
	s *pgstore.Store
}

// NewNodeReadyAdapter returns a NodeReadyAdapter backed by s.
func NewNodeReadyAdapter(s *pgstore.Store) *NodeReadyAdapter {
	return &NodeReadyAdapter{s: s}
}

func (a *NodeReadyAdapter) ListReadyNodes(ctx context.Context) ([]controller.ReadyNode, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var ready []controller.ReadyNode
	for _, n := range nodes {
		// A Node being drained takes no new Projects even if it was not
		// cordoned as well.
		if n.Status.State == v1.NodeStateReady && !n.Spec.Unschedulable && !n.Spec.Drain {
			ready = append(ready, controller.ReadyNode{
				Name:        n.Name,
				Labels:      n.Labels,
				Allocatable: n.Status.Allocatable,
				Conditions:  n.Status.Conditions,
			})
		}
	}
	return ready, nil
}
