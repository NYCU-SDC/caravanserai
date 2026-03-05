package controller

import (
	"context"
	"time"

	"NYCU-SDC/caravanserai/internal/event"
	"go.uber.org/zap"
)

// ProjectPhase mirrors api/v1.ProjectPhase.  Redeclared here for the same
// reason as NodeState: to keep this package free of circular imports until the
// store layer is in place.
type ProjectPhase string

const (
	ProjectPhasePending     ProjectPhase = "Pending"
	ProjectPhaseScheduled   ProjectPhase = "Scheduled"
	ProjectPhaseRunning     ProjectPhase = "Running"
	ProjectPhaseFailed      ProjectPhase = "Failed"
	ProjectPhaseTerminating ProjectPhase = "Terminating"
	ProjectPhaseTerminated  ProjectPhase = "Terminated"
)

// projectResyncInterval is how often the Seed loop re-enqueues all Pending
// projects as a fallback in case an event was dropped.
const projectResyncInterval = 30 * time.Second

// SchedulerProjectStore is the store surface needed by
// ProjectSchedulerController.
type SchedulerProjectStore interface {
	// ListProjectNamesByPhase returns the names of all Projects in the given phase.
	ListProjectNamesByPhase(ctx context.Context, phase ProjectPhase) ([]string, error)

	// GetProjectPhase returns the current phase and nodeRef of the named Project.
	GetProjectPhase(ctx context.Context, name string) (ProjectPhase, string, error)

	// SetProjectScheduled writes the nodeRef and transitions the Project to
	// Scheduled phase atomically.
	SetProjectScheduled(ctx context.Context, name, nodeRef string) error
}

// SchedulerNodeStore is the store surface needed to enumerate schedulable Nodes.
type SchedulerNodeStore interface {
	// ListReadyNodeNames returns the names of all Nodes in Ready state that
	// are not marked Unschedulable.
	ListReadyNodeNames(ctx context.Context) ([]string, error)
}

// ProjectSchedulerController picks a target Node for every Project in Pending
// phase and transitions it to Scheduled.
//
// MVP scheduling algorithm: select the first Ready Node in the list returned
// by the store.  A more sophisticated algorithm (resource-aware, affinity,
// weighted random) can be dropped in later without changing the controller
// lifecycle or the Manager wiring.
type ProjectSchedulerController struct {
	logger   *zap.Logger
	projects SchedulerProjectStore
	nodes    SchedulerNodeStore
	bus      *event.Bus
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
) *ProjectSchedulerController {
	return &ProjectSchedulerController{
		logger:   logger,
		projects: projects,
		nodes:    nodes,
		bus:      bus,
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
		return Result{}, err
	}

	if phase != ProjectPhasePending {
		log.Debug("Project is not Pending, nothing to do", zap.String("phase", string(phase)))
		return Result{}, nil
	}

	readyNodes, err := c.nodes.ListReadyNodeNames(ctx)
	if err != nil {
		return Result{}, err
	}

	if len(readyNodes) == 0 {
		log.Warn("No Ready nodes available, will retry")
		return Result{Requeue: true}, nil
	}

	// MVP: pick the first node. Replace with a real scoring algorithm later.
	target := readyNodes[0]

	log.Info("Scheduling project", zap.String("node", target))

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

	tick := time.NewTicker(projectResyncInterval)
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
	names, err := c.projects.ListProjectNamesByPhase(ctx, ProjectPhasePending)
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
