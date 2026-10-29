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

	// GetProjectSnapshot returns the named Project's phase and lifetime.
	GetProjectSnapshot(ctx context.Context, name string) (*ProjectSnapshot, error)

	// FinalizeProject deletes the record of lifetime uid if, at the moment of
	// deletion, the finalization predicate (v1.FinalizationBlocker) holds.
	// Returns store.ErrNotFound if it is gone, store.ErrStaleAssignment if
	// the name belongs to another lifetime, and *store.FinalizationBlockedError
	// if the predicate refuses.
	FinalizeProject(ctx context.Context, name, uid string) error

	// SetFinalizationBlocked records on a Project of lifetime uid that still
	// awaits finalization why its record is kept.
	SetFinalizationBlocked(ctx context.Context, name, uid, reason string) error
}

// ProjectTerminationController performs the final store deletion of Projects
// that have reached the Terminated phase, and of Terminating Projects with no
// current assignment — previously assigned Projects deleted while Pending,
// which no Agent will ever report Terminated.
//
// Flow:
//  1. The HTTP DELETE handler sets a Project to Terminating.
//  2. The Agent polls for Terminating projects, tears down Docker resources,
//     and reports Terminated via PATCH /status.
//  3. UpdateProjectStatus publishes a TopicProjectUpdated event.
//  4. This controller's Seed goroutine receives the event and enqueues the name.
//  5. Reconcile finalizes the record — deletes it only if the finalization
//     predicate holds on the row at the moment of deletion. A record it may
//     not delete yet stays, with FinalizationBlocked saying what it waits for;
//     the ResourcesRemoved report that unblocks it publishes an event that
//     brings it back here.
type ProjectTerminationController struct {
	logger       *zap.Logger
	projects     TerminationProjectStore
	bus          *event.Bus
	seedInterval time.Duration
}

// NewProjectTerminationController creates a ProjectTerminationController.
// bus may be nil; if so the controller relies solely on the periodic resync.
func NewProjectTerminationController(
	logger *zap.Logger,
	projects TerminationProjectStore,
	bus *event.Bus,
	opts ...Option,
) *ProjectTerminationController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = terminationResyncInterval
	}
	return &ProjectTerminationController{
		logger:       logger,
		projects:     projects,
		bus:          bus,
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *ProjectTerminationController) Name() string { return "project-termination" }

// Reconcile implements Controller.
//
// Whether the record may go is decided by the store, on the row it locks for
// the delete, not on the snapshot read here: the snapshot only says which
// lifetime the decision is for, so a same-name Project created in between is
// never the one deleted.
func (c *ProjectTerminationController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("project", name))

	p, err := c.projects.GetProjectSnapshot(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Already deleted — nothing to do.
			log.Debug("Project already gone, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	if !v1.AwaitsFinalization(v1.ProjectStatus{Phase: p.Phase, NodeRef: p.NodeRef}) {
		log.Debug("Project is not the server's to finalize, skipping",
			zap.String("phase", string(p.Phase)), zap.String("nodeRef", p.NodeRef))
		return Result{}, nil
	}

	err = c.projects.FinalizeProject(ctx, name, p.UID)
	var blocked *store.FinalizationBlockedError
	switch {
	case err == nil:
		log.Info("Project finalized")
		return Result{}, nil

	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStaleAssignment):
		// Finalized by a concurrent reconcile, or the name now belongs to a
		// newer lifetime that is not this decision's to delete.
		log.Debug("Project gone or replaced before finalizing, skipping", zap.Error(err))
		return Result{}, nil

	case errors.As(err, &blocked):
		// Not an error to retry: nothing changes until an obligation is
		// resolved, and that report publishes the event that re-enqueues this
		// Project. The periodic resync covers a dropped event.
		log.Info("Project cannot be finalized yet", zap.String("reason", blocked.Reason))
		if err := c.projects.SetFinalizationBlocked(ctx, name, p.UID, blocked.Reason); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrStaleAssignment) {
			return Result{}, err
		}
		return Result{}, nil

	default:
		return Result{}, err
	}
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

	tick := time.NewTicker(c.seedInterval)
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

// resyncTerminated lists every Project that may await finalization —
// Terminated, and Terminating, which Reconcile narrows to those with no current
// assignment — and enqueues each one.
func (c *ProjectTerminationController) resyncTerminated(ctx context.Context, enqueue func(name string)) {
	total := 0
	for _, phase := range []v1.ProjectPhase{v1.ProjectPhaseTerminated, v1.ProjectPhaseTerminating} {
		names, err := c.projects.ListProjectNamesByPhase(ctx, phase)
		if err != nil {
			c.logger.Error("Seed: failed to list projects", zap.Error(err),
				zap.String("phase", string(phase)), zap.String("controller", c.Name()))
			return
		}
		for _, name := range names {
			enqueue(name)
		}
		total += len(names)
	}
	if total > 0 {
		c.logger.Debug("Seed: enqueued projects awaiting finalization", zap.Int("count", total),
			zap.String("controller", c.Name()))
	}
}
