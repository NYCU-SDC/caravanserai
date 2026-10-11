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

func (a *NodeStoreAdapter) SetNodeState(ctx context.Context, name string, state v1.NodeState, reason, message string) error {
	node, err := a.s.GetNode(ctx, name)
	if err != nil {
		return err
	}
	node.Status.SetState(state, reason, message, time.Now().UTC())
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

func (a *NodeReadyAdapter) ListReadyNodes(ctx context.Context) ([]controller.ReadyNode, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var ready []controller.ReadyNode
	for _, n := range nodes {
		if n.Status.State == v1.NodeStateReady && !n.Spec.Unschedulable {
			ready = append(ready, controller.ReadyNode{
				Name:           n.Name,
				Labels:         n.Labels,
				Allocatable:    n.Status.Allocatable,
				Conditions:     n.Status.Conditions,
				RecentNotReady: n.Status.RecentNotReady,
			})
		}
	}
	return ready, nil
}
