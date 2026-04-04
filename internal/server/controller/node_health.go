package controller

import (
	"context"
	"errors"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

const (
	// NodeHeartbeatTimeout is the maximum age of a Node's LastHeartbeat before
	// it is considered unhealthy.  The Agent is expected to post a heartbeat
	// roughly every 30 s; three missed beats triggers NotReady.
	NodeHeartbeatTimeout = 90 * time.Second

	// nodeSeedInterval is how often the Seed loop re-enqueues all known Nodes
	// for a health check.  It should be shorter than NodeHeartbeatTimeout so
	// that stale nodes are detected promptly.
	nodeSeedInterval = 30 * time.Second
)

// NodeStore is the subset of the Store interface that NodeHealthController
// needs.  Using a narrow interface keeps the controller decoupled from the
// full store implementation and makes it trivial to mock in tests.
type NodeStore interface {
	// ListNodeNames returns the names of all known Nodes.
	ListNodeNames(ctx context.Context) ([]string, error)

	// GetNodeStatus returns the current status of the named Node.
	GetNodeStatus(ctx context.Context, name string) (NodeStatusSnapshot, error)

	// SetNodeState writes the aggregated state and updates the Ready condition.
	SetNodeState(ctx context.Context, name string, state v1.NodeState, reason, message string) error
}

// NodeStatusSnapshot is a minimal view of NodeStatus used by this controller.
type NodeStatusSnapshot struct {
	LastHeartbeat time.Time
	State         v1.NodeState
}

// NodeHealthController watches all Nodes and marks them NotReady when their
// heartbeat goes stale.
//
// Reconcile is called for every Node name enqueued by the Manager.  For
// MVP the seed goroutine in the Manager re-enqueues every Node periodically
// (timer-based); once a real Store watch is available, change events will
// drive reconciliation instead.
type NodeHealthController struct {
	logger       *zap.Logger
	store        NodeStore
	bus          *event.Bus
	clock        Clock
	seedInterval time.Duration
}

// NewNodeHealthController creates a NodeHealthController.
// store may be nil during early development; the controller will log a warning
// and become a no-op until a real store is injected.
// bus may be nil; if so, no node.updated events are published.
func NewNodeHealthController(logger *zap.Logger, store NodeStore, bus *event.Bus, opts ...Option) *NodeHealthController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = nodeSeedInterval
	}
	return &NodeHealthController{
		logger:       logger,
		store:        store,
		bus:          bus,
		clock:        o.clock,
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *NodeHealthController) Name() string { return "node-health" }

// Reconcile implements Controller.
func (c *NodeHealthController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("node", name))

	if c.store == nil {
		// TODO: remove once store is wired up.
		log.Warn("NodeStore not set, skipping reconcile")
		return Result{}, nil
	}

	snap, err := c.store.GetNodeStatus(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Node not found, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	age := c.clock.Since(snap.LastHeartbeat)

	switch {
	case snap.State == v1.NodeStateDraining:
		// Draining is an admin-set state; the health controller does not touch it.
		log.Debug("Node is draining, skipping health check")
		return Result{}, nil

	case age > NodeHeartbeatTimeout && snap.State != v1.NodeStateNotReady:
		log.Info("Node heartbeat timed out, marking NotReady",
			zap.Duration("age", age),
			zap.Duration("timeout", NodeHeartbeatTimeout),
		)
		if err := c.store.SetNodeState(ctx, name, v1.NodeStateNotReady,
			"HeartbeatTimeout",
			"Agent has not posted a heartbeat within the timeout window",
		); err != nil {
			return Result{}, err
		}
		// Notify ProjectReschedulerController (and any other subscribers) that
		// this node's state changed so they can react immediately.
		if c.bus != nil {
			c.bus.Publish(event.TopicNodeUpdated, name)
		}

	case age <= NodeHeartbeatTimeout && snap.State == v1.NodeStateNotReady:
		log.Info("Node heartbeat recovered, marking Ready",
			zap.Duration("age", age),
		)
		if err := c.store.SetNodeState(ctx, name, v1.NodeStateReady,
			"AgentReady",
			"Agent is healthy and posting heartbeats",
		); err != nil {
			return Result{}, err
		}

	default:
		log.Debug("Node health OK", zap.String("state", string(snap.State)), zap.Duration("heartbeat_age", age))
	}

	return Result{}, nil
}

// Seed implements controller.Seeder.
// It periodically lists all Node names from the store and enqueues each one
// for a health check.  This ensures the NodeHealthController reconciles every
// node even when no external event has triggered an enqueue.
func (c *NodeHealthController) Seed(ctx context.Context, enqueue func(name string)) {
	if c.store == nil {
		c.logger.Warn("NodeStore not set, Seed is a no-op", zap.String("controller", c.Name()))
		return
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	// Run once immediately on startup before the first tick.
	c.seedOnce(ctx, enqueue)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.seedOnce(ctx, enqueue)
		}
	}
}

func (c *NodeHealthController) seedOnce(ctx context.Context, enqueue func(name string)) {
	names, err := c.store.ListNodeNames(ctx)
	if err != nil {
		c.logger.Error("Seed: failed to list node names", zap.Error(err))
		return
	}
	for _, name := range names {
		enqueue(name)
	}
	c.logger.Debug("Seed: enqueued nodes", zap.Int("count", len(names)))
}
