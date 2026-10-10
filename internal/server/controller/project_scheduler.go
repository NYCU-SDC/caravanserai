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

// projectResyncInterval is how often the Seed loop re-enqueues all Pending
// projects as a fallback in case an event was dropped.
const projectResyncInterval = 30 * time.Second

// SchedulerProjectStore is the store surface needed by
// ProjectSchedulerController.
type SchedulerProjectStore interface {
	// ListProjectNamesByPhase returns the names of all Projects in the given phase.
	ListProjectNamesByPhase(ctx context.Context, phase v1.ProjectPhase) ([]string, error)

	// GetProjectPhase returns the current phase and nodeRef of the named Project.
	GetProjectPhase(ctx context.Context, name string) (v1.ProjectPhase, string, error)

	// GetProjectPlacement returns what the scheduler needs to know about the
	// named Project beyond its phase.
	GetProjectPlacement(ctx context.Context, name string) (ProjectPlacement, error)

	// CapacityProjectStore lets the scheduler's capacity ledger see what is
	// already placed on each Node.
	CapacityProjectStore

	// SetProjectScheduled writes the nodeRef and transitions the Project to
	// Scheduled phase atomically.
	SetProjectScheduled(ctx context.Context, name, nodeRef string) error
}

// ProjectPlacement is what the scheduler needs to know about a Project to
// choose a Node for it.
type ProjectPlacement struct {
	// Size is the declared size; empty when the Project declared none.
	Size v1.ProjectSize

	// ExcludedNodes are Nodes the Project must not be placed on: the ones it
	// failed on and was moved off, so that moving it changes Node.
	ExcludedNodes []string
}

// ReadyNode is the view of a schedulable Node that placement decisions need.
type ReadyNode struct {
	Name string

	// Labels are the Node's labels, including the cara.io/tier label when set.
	Labels map[string]string

	// Allocatable is the capacity the Node offers to Projects, as last
	// reported by its agent. Empty when the agent has not reported it.
	Allocatable v1.ResourceList

	// Conditions are the Node's observable conditions, e.g. DiskPressure.
	Conditions []v1.Condition
}

// SchedulerNodeStore is the store surface needed to enumerate schedulable Nodes.
type SchedulerNodeStore interface {
	// ListReadyNodes returns all Nodes in Ready state that are not marked
	// Unschedulable.
	ListReadyNodes(ctx context.Context) ([]ReadyNode, error)
}

// ProjectSchedulerController picks a target Node for every Project in Pending
// phase and transitions it to Scheduled.
//
// Scheduling is Filter -> Score. Filter narrows the Ready Nodes to those that
// can legitimately host the Project (see filterNodes); Score picks the best of
// them, preferring primary-tier Nodes and then the emptiest (see
// scoreCandidate). The Project is bound to the winner.
type ProjectSchedulerController struct {
	logger       *zap.Logger
	projects     SchedulerProjectStore
	nodes        SchedulerNodeStore
	ledger       *CapacityLedger
	bus          *event.Bus
	seedInterval time.Duration
}

// NewProjectSchedulerController creates a ProjectSchedulerController.
// Both store arguments may be nil during early development; the controller
// will log a warning and skip reconciliation until they are injected.
// bus may be nil; if so the controller relies solely on the resync fallback.
func NewProjectSchedulerController(
	logger *zap.Logger,
	projects SchedulerProjectStore,
	nodes SchedulerNodeStore,
	bus *event.Bus,
	opts ...Option,
) *ProjectSchedulerController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = projectResyncInterval
	}
	return &ProjectSchedulerController{
		logger:       logger,
		projects:     projects,
		nodes:        nodes,
		ledger:       NewCapacityLedger(logger, projects),
		bus:          bus,
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *ProjectSchedulerController) Name() string { return "project-scheduler" }

// Reconcile implements Controller.
//
// name is the name of a Project that may need scheduling.  If the Project is
// no longer Pending (e.g. it was already scheduled by a concurrent reconcile)
// the call is a no-op.
func (c *ProjectSchedulerController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("project", name))

	if c.projects == nil || c.nodes == nil {
		// TODO: remove once store is wired up.
		log.Warn("Store not set, skipping reconcile")
		return Result{}, nil
	}

	phase, _, err := c.projects.GetProjectPhase(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project not found, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	if phase != v1.ProjectPhasePending {
		log.Debug("Project is not Pending, nothing to do", zap.String("phase", string(phase)))
		return Result{}, nil
	}

	readyNodes, err := c.nodes.ListReadyNodes(ctx)
	if err != nil {
		return Result{}, err
	}

	if len(readyNodes) == 0 {
		log.Warn("No Ready nodes available, will retry")
		return Result{Requeue: true}, nil
	}

	placement, err := c.projects.GetProjectPlacement(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project not found, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	size := placement.Size
	candidates, rejected, err := filterNodes(ctx, c.ledger, readyNodes, size, placement.ExcludedNodes)
	if err != nil {
		return Result{}, err
	}

	if len(candidates) == 0 {
		log.Warn("No node can host the project, will retry",
			zap.String("size", string(v1.ProjectSpec{Size: size}.EffectiveSize())),
			zap.Strings("rejected", rejected))
		return Result{Requeue: true}, nil
	}

	best, score := pickBest(candidates, size)
	target := best.Name

	log.Info("Scheduling project",
		zap.String("node", target),
		zap.String("tier", string(v1.NodeTierFromLabels(best.Labels))),
		zap.Float64("score", score),
		zap.Int("candidates", len(candidates)))

	if err := c.projects.SetProjectScheduled(ctx, name, target); err != nil {
		return Result{}, err
	}

	log.Info("Project scheduled successfully", zap.String("node", target))
	return Result{}, nil
}

// Seed implements controller.Seeder.
//
// It has two sources of work:
//  1. Event bus: subscribes to TopicProjectCreated so newly created Pending
//     projects are enqueued immediately (fast path).
//  2. Resync ticker: every projectResyncInterval it lists all Pending projects
//     and enqueues them (fallback for any events that were dropped or for
//     projects that were Pending before this controller started).
func (c *ProjectSchedulerController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	if c.projects == nil {
		log.Warn("ProjectStore not set, Seed is a no-op")
		return
	}

	// Subscribe to project.created events if a bus is available.
	var created event.Handler
	if c.bus != nil {
		created = c.bus.Subscribe(event.TopicProjectCreated)
		log.Debug("Subscribed to project.created events")
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	// Run one resync immediately so any pre-existing Pending projects are
	// picked up before the first tick fires.
	c.resyncPending(ctx, enqueue)

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-created:
			if !ok {
				// Channel was closed; fall back to ticker-only mode.
				created = nil
				continue
			}
			log.Debug("Received project.created event", zap.String("project", e.Name))
			enqueue(e.Name)

		case <-tick.C:
			c.resyncPending(ctx, enqueue)
		}
	}
}

// resyncPending lists all Pending projects and enqueues each one.
func (c *ProjectSchedulerController) resyncPending(ctx context.Context, enqueue func(name string)) {
	names, err := c.projects.ListProjectNamesByPhase(ctx, v1.ProjectPhasePending)
	if err != nil {
		c.logger.Error("Seed: failed to list pending projects", zap.Error(err),
			zap.String("controller", c.Name()))
		return
	}
	for _, name := range names {
		enqueue(name)
	}
	c.logger.Debug("Seed: enqueued pending projects", zap.Int("count", len(names)),
		zap.String("controller", c.Name()))
}
