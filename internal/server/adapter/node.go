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
		State:         controller.NodeState(node.Status.State),
	}, nil
}

func (a *NodeStoreAdapter) SetNodeState(ctx context.Context, name string, state controller.NodeState, reason, message string) error {
	node, err := a.s.GetNode(ctx, name)
	if err != nil {
		return err
	}
	node.Status.State = v1.NodeState(state)
	// Update or append the Ready condition.
	condStatus := v1.ConditionTrue
	if state != controller.NodeStateReady {
		condStatus = v1.ConditionFalse
	}
	now := time.Now().UTC()
	updated := false
	for i, c := range node.Status.Conditions {
		if c.Type == v1.ConditionTypeReady {
			node.Status.Conditions[i] = v1.Condition{
				Type:               v1.ConditionTypeReady,
				Status:             condStatus,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
			}
			updated = true
			break
		}
	}
	if !updated {
		node.Status.Conditions = append(node.Status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeReady,
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		})
	}
	return a.s.UpdateNodeStatus(ctx, name, node.Status)
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

func (a *NodeReadyAdapter) ListReadyNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range nodes {
		if n.Status.State == v1.NodeStateReady && !n.Spec.Unschedulable {
			names = append(names, n.Name)
		}
	}
	return names, nil
}
