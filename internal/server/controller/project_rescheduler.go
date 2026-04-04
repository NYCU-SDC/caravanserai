package controller

import (
	"context"
	"errors"
	"time"

	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

const (
	// terminatingTimeout is the maximum time a Project is allowed to stay in
	// Terminating phase after its node went NotReady before the rescheduler
	// forcefully transitions it to Terminated.  Docker resources on the dead
	// node may be left behind; operators are expected to clean those up
	// manually.
	terminatingTimeout = 10 * time.Minute

	// reschedulerResyncInterval is how often the Seed loop re-enqueues all
	// NotReady nodes as a fallback in case an event was dropped.
	reschedulerResyncInterval = 30 * time.Second

	// runningGracePeriod is the maximum time a Running project on a NotReady
	// node is allowed before the rescheduler resets it to Pending.  This gives
	// transient network issues time to resolve before the project is moved to
	// another node, avoiding unnecessary churn and split-brain risk for
	// stateful workloads.  Set to 3× the 90-second heartbeat timeout.
	runningGracePeriod = 3 * time.Minute

	// condTypeTerminatingAt is the Condition.Type written by the rescheduler
	// when it first observes a Terminating project whose node is NotReady.
	// Its LastTransitionTime serves as the start of the force-termination
	// timeout clock.
	condTypeTerminatingAt = "TerminatingAt"

	// condTypeNotReadyAt is the Condition.Type written by the rescheduler
	// when it first observes a Running project whose node is NotReady.
	// Its LastTransitionTime serves as the start of the running grace
	// period clock.
	condTypeNotReadyAt = "NotReadyAt"
)

// ProjectSnapshot is the minimal view of a Project needed by
// ProjectReschedulerController.
type ProjectSnapshot struct {
	Name       string
	Phase      ProjectPhase
	NodeRef    string
	Conditions []ConditionSnapshot
}

// ConditionSnapshot is the minimal view of a Condition needed by this
// controller.
type ConditionSnapshot struct {
	Type               string
	LastTransitionTime time.Time
}

// ReschedulerProjectStore is the store surface needed by
// ProjectReschedulerController.
type ReschedulerProjectStore interface {
	// ListProjectsByNodeRef returns all Projects assigned to nodeRef whose
	// phase is one of phases.
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []ProjectPhase) ([]*ProjectSnapshot, error)

	// SetProjectPending clears the nodeRef, sets phase=Pending, and records a
	// Phase condition with reason=NodeNotReady.
	SetProjectPending(ctx context.Context, name string) error

	// SetTerminatingAt writes (or replaces) a TerminatingAt condition on the
	// Project to record when the rescheduler first observed the node as
	// NotReady.  This timestamp is used to calculate the force-termination
	// timeout.
	SetTerminatingAt(ctx context.Context, name string, at time.Time) error

	// SetNotReadyAt writes (or replaces) a NotReadyAt condition on the
	// Project to record when the rescheduler first observed the node as
	// NotReady while the project was Running.  This timestamp is used to
	// calculate the running grace period before resetting to Pending.
	SetNotReadyAt(ctx context.Context, name string, at time.Time) error

	// ForceTerminated transitions the Project to Terminated phase and records a
	// Phase condition with reason=TerminationTimeout.  The
	// ProjectTerminationController will delete the record shortly after.
	ForceTerminated(ctx context.Context, name string) error
}

// ReschedulerNodeStore is the store surface needed to check node state.
type ReschedulerNodeStore interface {
	// GetNodeStatus returns the current status of the named Node.
	GetNodeStatus(ctx context.Context, name string) (NodeStatusSnapshot, error)

	// ListNotReadyNodeNames returns the names of all Nodes currently in
	// NotReady state.
	ListNotReadyNodeNames(ctx context.Context) ([]string, error)
}

// ProjectReschedulerController reacts to nodes going NotReady and reschedules
// or force-terminates the Projects that were running on them.
//
// For Scheduled/Running projects:
//   - Reset to Pending so the ProjectSchedulerController can place them on a
//     healthy node.
//
// For Terminating projects (node died mid-teardown):
//   - First observation: record TerminatingAt condition to start the clock.
//   - Subsequent observations: once the TerminatingAt age exceeds
//     terminatingTimeout, force-transition to Terminated so that
//     ProjectTerminationController can delete the DB record.
type ProjectReschedulerController struct {
	logger   *zap.Logger
	projects ReschedulerProjectStore
	nodes    ReschedulerNodeStore
	bus      *event.Bus
	clock    Clock
}

// NewProjectReschedulerController creates a ProjectReschedulerController.
// bus may be nil; if so the controller relies solely on the periodic resync.
func NewProjectReschedulerController(
	logger *zap.Logger,
	projects ReschedulerProjectStore,
	nodes ReschedulerNodeStore,
	bus *event.Bus,
	opts ...Option,
) *ProjectReschedulerController {
	o := applyOptions(opts)
	return &ProjectReschedulerController{
		logger:   logger,
		projects: projects,
		nodes:    nodes,
		bus:      bus,
		clock:    o.clock,
	}
}

// Name implements Controller.
func (c *ProjectReschedulerController) Name() string { return "project-rescheduler" }

// Reconcile implements Controller.
//
// name is the name of a Node that has (or may have) gone NotReady.  The
// controller verifies the node is still NotReady before acting to avoid racing
// with a recovery event.
func (c *ProjectReschedulerController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("node", name))

	snap, err := c.nodes.GetNodeStatus(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Node was deleted; nothing to do.
			log.Debug("Node not found, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	if snap.State != NodeStateNotReady {
		log.Debug("Node is not NotReady, nothing to do", zap.String("state", string(snap.State)))
		return Result{}, nil
	}

	projects, err := c.projects.ListProjectsByNodeRef(ctx, name, []ProjectPhase{
		ProjectPhaseScheduled,
		ProjectPhaseRunning,
		ProjectPhaseTerminating,
	})
	if err != nil {
		return Result{}, err
	}

	if len(projects) == 0 {
		log.Debug("No projects on NotReady node")
		return Result{}, nil
	}

	var requeueNeeded bool

	for _, p := range projects {
		switch p.Phase {
		case ProjectPhaseScheduled:
			log.Info("Resetting scheduled project to Pending due to NotReady node",
				zap.String("project", p.Name),
			)
			if err := c.projects.SetProjectPending(ctx, p.Name); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					log.Debug("Project disappeared before reset, skipping", zap.String("project", p.Name))
					continue
				}
				return Result{}, err
			}
			log.Info("Scheduled project reset to Pending", zap.String("project", p.Name))

		case ProjectPhaseRunning:
			requeue, err := c.handleRunning(ctx, log, p)
			if err != nil {
				return Result{}, err
			}
			if requeue {
				requeueNeeded = true
			}

		case ProjectPhaseTerminating:
			requeue, err := c.handleTerminating(ctx, log, p)
			if err != nil {
				return Result{}, err
			}
			if requeue {
				requeueNeeded = true
			}
		}
	}

	if requeueNeeded {
		// At least one Terminating project is waiting for its timeout to
		// expire.  Requeue this node so we check again after the manager's
		// defaultRequeueAfter interval.
		return Result{Requeue: true}, nil
	}
	return Result{}, nil
}

// handleTerminating processes a single Terminating project on a NotReady node.
// Returns (true, nil) if the project still needs to be checked again later.
func (c *ProjectReschedulerController) handleTerminating(
	ctx context.Context,
	log *zap.Logger,
	p *ProjectSnapshot,
) (requeue bool, err error) {
	log = log.With(zap.String("project", p.Name))

	// Find the TerminatingAt condition, if any.
	var terminatingAt time.Time
	var found bool
	for _, cond := range p.Conditions {
		if cond.Type == condTypeTerminatingAt {
			terminatingAt = cond.LastTransitionTime
			found = true
			break
		}
	}

	if !found {
		// First time we see this Terminating project on a NotReady node.
		// Record the current time as the start of the timeout clock.
		now := c.clock.Now().UTC()
		log.Info("Recording TerminatingAt timestamp for stranded project", zap.Time("at", now))
		if err := c.projects.SetTerminatingAt(ctx, p.Name, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				log.Debug("Project disappeared before TerminatingAt write, skipping")
				return false, nil
			}
			return false, err
		}
		// Come back after the timeout to check.
		return true, nil
	}

	elapsed := c.clock.Since(terminatingAt)
	if elapsed < terminatingTimeout {
		remaining := terminatingTimeout - elapsed
		log.Info("Terminating project waiting for timeout",
			zap.Duration("elapsed", elapsed),
			zap.Duration("remaining", remaining),
		)
		return true, nil
	}

	// Timeout exceeded — force to Terminated.
	log.Info("Termination timeout exceeded, forcing project to Terminated",
		zap.Duration("elapsed", elapsed),
		zap.Duration("timeout", terminatingTimeout),
	)
	if err := c.projects.ForceTerminated(ctx, p.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project disappeared before ForceTerminated, skipping")
			return false, nil
		}
		return false, err
	}
	log.Info("Project force-terminated", zap.String("project", p.Name))
	return false, nil
}

// handleRunning processes a single Running project on a NotReady node.
// Returns (true, nil) if the project still needs to be checked again later.
func (c *ProjectReschedulerController) handleRunning(
	ctx context.Context,
	log *zap.Logger,
	p *ProjectSnapshot,
) (requeue bool, err error) {
	log = log.With(zap.String("project", p.Name))

	// Find the NotReadyAt condition, if any.
	var notReadyAt time.Time
	var found bool
	for _, cond := range p.Conditions {
		if cond.Type == condTypeNotReadyAt {
			notReadyAt = cond.LastTransitionTime
			found = true
			break
		}
	}

	if !found {
		// First time we see this Running project on a NotReady node.
		// Record the current time as the start of the grace period clock.
		now := c.clock.Now().UTC()
		log.Info("Recording NotReadyAt timestamp for stranded running project", zap.Time("at", now))
		if err := c.projects.SetNotReadyAt(ctx, p.Name, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				log.Debug("Project disappeared before NotReadyAt write, skipping")
				return false, nil
			}
			return false, err
		}
		// Come back after the grace period to check.
		return true, nil
	}

	elapsed := c.clock.Since(notReadyAt)
	if elapsed < runningGracePeriod {
		remaining := runningGracePeriod - elapsed
		log.Info("Running project waiting for grace period",
			zap.Duration("elapsed", elapsed),
			zap.Duration("remaining", remaining),
		)
		return true, nil
	}

	// Grace period exceeded — reset to Pending.
	log.Info("Running grace period exceeded, resetting project to Pending",
		zap.Duration("elapsed", elapsed),
		zap.Duration("gracePeriod", runningGracePeriod),
	)
	if err := c.projects.SetProjectPending(ctx, p.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project disappeared before SetProjectPending, skipping")
			return false, nil
		}
		return false, err
	}
	log.Info("Running project reset to Pending", zap.String("project", p.Name))
	return false, nil
}

// Seed implements controller.Seeder.
//
// Work sources:
//  1. TopicNodeUpdated events from the event bus (fast path — immediate
//     reaction when NodeHealthController marks a node NotReady).
//  2. Periodic resync — every reschedulerResyncInterval it lists all NotReady
//     nodes and enqueues them, catching any events that were dropped and any
//     nodes that were already NotReady before this controller started.
func (c *ProjectReschedulerController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var nodeUpdated event.Handler
	if c.bus != nil {
		nodeUpdated = c.bus.Subscribe(event.TopicNodeUpdated)
		log.Debug("Subscribed to node.updated events")
	}

	tick := time.NewTicker(reschedulerResyncInterval)
	defer tick.Stop()

	// Run one resync immediately to handle nodes that were already NotReady
	// before this controller started.
	c.resyncNotReadyNodes(ctx, enqueue)

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-nodeUpdated:
			if !ok {
				nodeUpdated = nil
				continue
			}
			log.Debug("Received node.updated event", zap.String("node", e.Name))
			enqueue(e.Name)

		case <-tick.C:
			c.resyncNotReadyNodes(ctx, enqueue)
		}
	}
}

// resyncNotReadyNodes lists all NotReady nodes and enqueues each one.
func (c *ProjectReschedulerController) resyncNotReadyNodes(ctx context.Context, enqueue func(name string)) {
	names, err := c.nodes.ListNotReadyNodeNames(ctx)
	if err != nil {
		c.logger.Error("Seed: failed to list NotReady nodes", zap.Error(err),
			zap.String("controller", c.Name()))
		return
	}
	for _, name := range names {
		enqueue(name)
	}
	if len(names) > 0 {
		c.logger.Debug("Seed: enqueued NotReady nodes", zap.Int("count", len(names)),
			zap.String("controller", c.Name()))
	}
}
