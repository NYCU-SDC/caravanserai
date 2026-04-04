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

// terminationResyncInterval is how often the Seed loop re-enqueues all
// Terminated projects as a fallback in case an event was dropped.
const terminationResyncInterval = 30 * time.Second

// TerminationProjectStore is the store surface needed by
// ProjectTerminationController.
type TerminationProjectStore interface {
	// ListProjectNamesByPhase returns the names of all Projects in the given phase.
	ListProjectNamesByPhase(ctx context.Context, phase v1.ProjectPhase) ([]string, error)

	// GetProjectPhase returns the current phase of the named Project.
	GetProjectPhase(ctx context.Context, name string) (v1.ProjectPhase, string, error)

	// DeleteProject removes the Project record from the store.
	// Returns store.ErrNotFound if the project no longer exists (safe to ignore).
	DeleteProject(ctx context.Context, name string) error
}

// ProjectTerminationController performs the final store deletion of Projects
// that have reached the Terminated phase.
//
// Flow:
//  1. The HTTP DELETE handler sets a Project to Terminating.
//  2. The Agent polls for Terminating projects, tears down Docker resources,
//     and reports Terminated via PATCH /status.
//  3. UpdateProjectStatus publishes a TopicProjectUpdated event.
//  4. This controller's Seed goroutine receives the event and enqueues the name.
//  5. Reconcile verifies the phase is still Terminated and deletes the record.
type ProjectTerminationController struct {
	logger   *zap.Logger
	projects TerminationProjectStore
	bus      *event.Bus
}

// NewProjectTerminationController creates a ProjectTerminationController.
// bus may be nil; if so the controller relies solely on the periodic resync.
func NewProjectTerminationController(
	logger *zap.Logger,
	projects TerminationProjectStore,
	bus *event.Bus,
) *ProjectTerminationController {
	return &ProjectTerminationController{
		logger:   logger,
		projects: projects,
		bus:      bus,
	}
}

// Name implements Controller.
func (c *ProjectTerminationController) Name() string { return "project-termination" }

// Reconcile implements Controller.
//
// It checks that the named project is still in Terminated phase (a concurrent
// operation may have already removed it) and then deletes it from the store.
func (c *ProjectTerminationController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("project", name))

	phase, _, err := c.projects.GetProjectPhase(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Already deleted — nothing to do.
			log.Debug("Project already gone, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	if phase != v1.ProjectPhaseTerminated {
		log.Debug("Project is not Terminated, skipping", zap.String("phase", string(phase)))
		return Result{}, nil
	}

	log.Info("Deleting Terminated project from store")

	if err := c.projects.DeleteProject(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Already deleted by a concurrent reconcile.
			log.Debug("Project already gone during delete, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	log.Info("Project deleted successfully")
	return Result{}, nil
}

// Seed implements controller.Seeder.
//
// It subscribes to TopicProjectUpdated so that every status update is checked
// immediately for the Terminated phase.  A periodic resync ensures that any
// dropped events are eventually processed.
func (c *ProjectTerminationController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var updated event.Handler
	if c.bus != nil {
		updated = c.bus.Subscribe(event.TopicProjectUpdated)
		log.Debug("Subscribed to project.updated events")
	}

	tick := time.NewTicker(terminationResyncInterval)
	defer tick.Stop()

	// Run one resync immediately on startup to catch any pre-existing
	// Terminated projects.
	c.resyncTerminated(ctx, enqueue)

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-updated:
			if !ok {
				updated = nil
				continue
			}
			log.Debug("Received project.updated event", zap.String("project", e.Name))
			enqueue(e.Name)

		case <-tick.C:
			c.resyncTerminated(ctx, enqueue)
		}
	}
}

// resyncTerminated lists all Terminated projects and enqueues each one.
func (c *ProjectTerminationController) resyncTerminated(ctx context.Context, enqueue func(name string)) {
	names, err := c.projects.ListProjectNamesByPhase(ctx, v1.ProjectPhaseTerminated)
	if err != nil {
		c.logger.Error("Seed: failed to list terminated projects", zap.Error(err),
			zap.String("controller", c.Name()))
		return
	}
	for _, name := range names {
		enqueue(name)
	}
	if len(names) > 0 {
		c.logger.Debug("Seed: enqueued terminated projects", zap.Int("count", len(names)),
			zap.String("controller", c.Name()))
	}
}
