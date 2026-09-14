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
)

// ProjectSnapshot is the minimal view of a Project needed by
// ProjectReschedulerController.
type ProjectSnapshot struct {
	Name       string
	Phase      v1.ProjectPhase
	NodeRef    string
	Conditions []ConditionSnapshot
}

// ConditionSnapshot is the minimal view of a Condition needed by this
// controller.
type ConditionSnapshot struct {
	Type               v1.ConditionType
	LastTransitionTime time.Time
}

// ReschedulerProjectStore is the store surface needed by
// ProjectReschedulerController.
type ReschedulerProjectStore interface {
	// ListProjectsByNodeRef returns all Projects assigned to nodeRef whose
	// phase is one of phases.
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*ProjectSnapshot, error)

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

	// ClearRescheduleClocks removes the NotReadyAt and TerminatingAt
	// conditions from the Project without touching its phase.  Called when the
	// node recovers before either clock expires.
	//
	// It must ignore a Project no longer assigned to nodeRef, and a clock
	// stamped after notAfter — that one belongs to an outage which began after
	// this cleanup was decided on, and deleting it would restart a timeout
	// that is legitimately running.
	ClearRescheduleClocks(ctx context.Context, name, nodeRef string, notAfter time.Time) error
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
	logger       *zap.Logger
	projects     ReschedulerProjectStore
	nodes        ReschedulerNodeStore
	bus          *event.Bus
	clock        Clock
	seedInterval time.Duration
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
	interval := o.seedInterval
	if interval == 0 {
		interval = reschedulerResyncInterval
	}
	return &ProjectReschedulerController{
		logger:       logger,
		projects:     projects,
		nodes:        nodes,
		bus:          bus,
		clock:        o.clock,
		seedInterval: interval,
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

	if snap.State != v1.NodeStateNotReady {
		// The node is healthy. Either it never went NotReady, or it came back
		// before a clock expired — and in that second case the Projects stayed
		// put and their phases never changed, so nothing else in the system
		// will remove the clocks this controller started on them.
		//
		// The clocks are already harmless by then: both handlers ignore one
		// older than the node's last heartbeat. What is left is Project status
		// reporting a failure that is over, which is worth clearing but is not
		// what correctness rests on — this path runs on a single node.updated
		// event, and event.Bus.Publish drops events when a subscriber is
		// behind.
		log.Debug("Node is not NotReady, clearing any reschedule clocks",
			zap.String("state", string(snap.State)))
		return Result{}, c.clearRescheduleClocks(ctx, log, name, snap.LastHeartbeat)
	}

	projects, err := c.projects.ListProjectsByNodeRef(ctx, name, []v1.ProjectPhase{
		v1.ProjectPhaseScheduled,
		v1.ProjectPhaseRunning,
		v1.ProjectPhaseTerminating,
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
		case v1.ProjectPhaseScheduled:
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

		case v1.ProjectPhaseRunning:
			requeue, err := c.handleRunning(ctx, log, p, snap.LastHeartbeat)
			if err != nil {
				return Result{}, err
			}
			if requeue {
				requeueNeeded = true
			}

		case v1.ProjectPhaseTerminating:
			requeue, err := c.handleTerminating(ctx, log, p, snap.LastHeartbeat)
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

// clearRescheduleClocks removes the reschedule clocks from every Project still
// assigned to a node that is no longer NotReady.
//
// Only Projects that actually carry a clock are written. This runs on every
// node.updated event for a healthy node, which is the common case, and the
// common case must not cost a write per Project per event.
//
// notAfter is the heartbeat that proved the node healthy; the store refuses to
// delete a clock stamped later than it, so a cleanup overtaken by the next
// outage cannot delete that outage's clock.
//
// A failure on one Project does not abandon the rest. These are stale records,
// not state anything is waiting on, so clearing as many as possible and
// reporting that something was missed is the useful outcome — the node is
// healthy and there is no deadline left to race.
func (c *ProjectReschedulerController) clearRescheduleClocks(
	ctx context.Context,
	log *zap.Logger,
	nodeName string,
	notAfter time.Time,
) error {
	projects, err := c.projects.ListProjectsByNodeRef(ctx, nodeName, []v1.ProjectPhase{
		v1.ProjectPhaseScheduled,
		v1.ProjectPhaseRunning,
		v1.ProjectPhaseTerminating,
	})
	if err != nil {
		return err
	}

	var firstErr error
	for _, p := range projects {
		if !hasRescheduleClock(p) {
			continue
		}
		log.Info("Node recovered, clearing reschedule clock", zap.String("project", p.Name))
		if err := c.projects.ClearRescheduleClocks(ctx, p.Name, nodeName, notAfter); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				log.Debug("Project disappeared before clock clear, skipping",
					zap.String("project", p.Name))
				continue
			}
			log.Error("Failed to clear reschedule clock",
				zap.String("project", p.Name), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// hasRescheduleClock reports whether p carries either reschedule clock.
func hasRescheduleClock(p *ProjectSnapshot) bool {
	for _, cond := range p.Conditions {
		if cond.Type == v1.ConditionTypeNotReadyAt || cond.Type == v1.ConditionTypeTerminatingAt {
			return true
		}
	}
	return false
}

// incidentClock finds the clock condition of type t and reports whether it is
// running for the node's current unhealthy period.
//
// startedAt is whatever timestamp was found, zero when there is no such
// condition. running is false in both the absent case and the stale case, so a
// caller that gets false starts the clock; startedAt tells it which of the two
// happened, for the log.
//
// A clock started before the node's most recent successful heartbeat was
// started during an earlier failure that the node has since recovered from.
// Reading it would measure this failure from that one — however many hours or
// days old — so the wait it governs would be skipped entirely, and would
// protect each Project exactly once before silently stopping.
//
// Deciding this from the node's own heartbeat rather than from a cleanup step
// is deliberate: it holds even when nothing removed the stale condition.
// Removing it is worth doing so that status stops reporting a failure that is
// over, but it must not be what correctness rests on.
func incidentClock(
	conds []ConditionSnapshot,
	t v1.ConditionType,
	lastHeartbeat time.Time,
) (startedAt time.Time, running bool) {
	for _, cond := range conds {
		if cond.Type != t {
			continue
		}
		return cond.LastTransitionTime, !cond.LastTransitionTime.Before(lastHeartbeat)
	}
	return time.Time{}, false
}

// handleTerminating processes a single Terminating project on a NotReady node.
// Returns (true, nil) if the project still needs to be checked again later.
// The timeout it governs ends in force-termination, which declares the Project
// Terminated without the agent confirming teardown and may strand Docker
// resources on the node. That is the reason the clock must belong to this
// outage: a node that recovers, starts a slow teardown and fails again part-way
// would otherwise be force-terminated against the previous outage's clock,
// reaching the destructive outcome without the wait that exists to avoid it.
func (c *ProjectReschedulerController) handleTerminating(
	ctx context.Context,
	log *zap.Logger,
	p *ProjectSnapshot,
	lastHeartbeat time.Time,
) (requeue bool, err error) {
	log = log.With(zap.String("project", p.Name))

	terminatingAt, running := incidentClock(p.Conditions, v1.ConditionTypeTerminatingAt, lastHeartbeat)

	if !running {
		if terminatingAt.IsZero() {
			log.Info("Recording TerminatingAt timestamp for stranded project")
		} else {
			log.Info("Ignoring a TerminatingAt from an earlier outage, restarting the timeout",
				zap.Time("staleClock", terminatingAt), zap.Time("lastHeartbeat", lastHeartbeat))
		}

		now := c.clock.Now().UTC()
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
//
// lastHeartbeat is the node's most recent heartbeat. It is what separates a
// clock started during this failure from one left over by an earlier one, and
// so what makes the grace period repeatable rather than a once-per-Project
// protection. See incidentClock.
func (c *ProjectReschedulerController) handleRunning(
	ctx context.Context,
	log *zap.Logger,
	p *ProjectSnapshot,
	lastHeartbeat time.Time,
) (requeue bool, err error) {
	log = log.With(zap.String("project", p.Name))

	notReadyAt, running := incidentClock(p.Conditions, v1.ConditionTypeNotReadyAt, lastHeartbeat)

	if !running {
		if notReadyAt.IsZero() {
			log.Info("Recording NotReadyAt timestamp for stranded running project")
		} else {
			log.Info("Ignoring a NotReadyAt from an earlier incident, restarting the grace period",
				zap.Time("staleClock", notReadyAt), zap.Time("lastHeartbeat", lastHeartbeat))
		}

		now := c.clock.Now().UTC()
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

	tick := time.NewTicker(c.seedInterval)
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
