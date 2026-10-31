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
	// terminatingTimeout is the default for how long a Terminating Project's
	// node may stay NotReady before the rescheduler marks the Project
	// TerminationBlocked (see WithTerminatingTimeout). Expiry never finalizes
	// the Project: only its node can confirm that the resources are gone, and
	// the record has to outlive them.
	terminatingTimeout = 10 * time.Minute

	// reschedulerResyncInterval is how often the Seed loop re-enqueues all
	// NotReady nodes as a fallback in case an event was dropped.
	reschedulerResyncInterval = 30 * time.Second

	// runningGracePeriod is the maximum time a Running project on a NotReady
	// node is allowed before the rescheduler tries to release it.  This gives
	// transient network issues time to resolve before the project is moved to
	// another node, avoiding unnecessary churn and split-brain risk for
	// stateful workloads.  Set to 3× the 90-second heartbeat timeout.
	runningGracePeriod = 3 * time.Minute
)

// ProjectSnapshot is the minimal view of a Project needed by
// ProjectReschedulerController and CapacityLedger.
type ProjectSnapshot struct {
	Name                 string
	UID                  string
	Phase                v1.ProjectPhase
	NodeRef              string
	AssignmentGeneration int64
	Size                 v1.ProjectSize
	// FailedNodes are the Nodes the Project has been moved off after Failed.
	FailedNodes []string
	Conditions  []ConditionSnapshot
}

// ConditionSnapshot is the minimal view of a Condition needed by this
// controller.
type ConditionSnapshot struct {
	Type               v1.ConditionType
	Reason             string
	LastTransitionTime time.Time
}

// ReschedulerProjectStore is the store surface needed by
// ProjectReschedulerController.
type ReschedulerProjectStore interface {
	// ListProjectsByNodeRef returns all Projects assigned to nodeRef whose
	// phase is one of phases.
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*ProjectSnapshot, error)

	// ReleaseAssignment gives up p's assignment — the UID, nodeRef and
	// generation in the snapshot — so the Project can be scheduled again, but
	// only with an accepted release authorization. Today that is WorkloadStopped
	// evidence for exactly that assignment.
	//
	// With it, one write clears nodeRef, sets phase=Pending with a Phase
	// condition of reason=NodeNotReady, and appends the assignment's cleanup
	// obligation; released is true. Without it, the Project keeps its nodeRef
	// and phase and is marked TakeoverRequired=True; released is false.
	// Re-marking an already marked Project writes nothing.
	//
	// Returns ErrStaleAssignment when the Project no longer holds the snapshot's
	// assignment, ErrConflictState when it is no longer Scheduled or Running,
	// and ErrNotFound when it is gone.
	ReleaseAssignment(ctx context.Context, p *ProjectSnapshot) (released bool, err error)

	// SetTerminatingAt writes (or replaces) a TerminatingAt condition on the
	// Project to record when the rescheduler first observed the node as
	// NotReady.  This timestamp is used to calculate the termination timeout.
	SetTerminatingAt(ctx context.Context, name string, at time.Time) error

	// SetNotReadyAt writes (or replaces) a NotReadyAt condition on the
	// Project to record when the rescheduler first observed the node as
	// NotReady while the project was Running.  This timestamp is used to
	// calculate the running grace period before trying to release it.
	SetNotReadyAt(ctx context.Context, name string, at time.Time) error

	// SetTerminationBlocked writes (or replaces) a TerminationBlocked condition
	// stamped at, on a Project that is still Terminating. The phase does not
	// change: the Project stays Terminating until its node returns and reports
	// the resources removed.
	SetTerminationBlocked(ctx context.Context, name, nodeRef string, at time.Time) error

	// ClearRescheduleClocks removes the outage conditions (see
	// IsOutageCondition) from the Project without touching its phase. Called
	// when the node recovers before the Project was released.
	//
	// It must ignore a Project no longer assigned to nodeRef, and a condition
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

// ProjectReschedulerController reacts to nodes going NotReady: it reschedules
// the Projects that were running on them, and reports the deletions they hold
// up.
//
// For Scheduled/Running projects:
//   - Release the assignment so the ProjectSchedulerController can place them
//     on a healthy node — but only when the release is authorized (ADR-0017).
//     Without authorization the Project stays on the NotReady node, marked
//     TakeoverRequired, and the release is retried on later reconciles.
//
// For Terminating projects (node died mid-teardown):
//   - First observation: record TerminatingAt condition to start the clock.
//   - Once the TerminatingAt age exceeds the termination timeout, mark the Project
//     TerminationBlocked. It stays Terminating: the record is deleted only
//     after the node confirms its resources are removed.
type ProjectReschedulerController struct {
	logger       *zap.Logger
	projects     ReschedulerProjectStore
	nodes        ReschedulerNodeStore
	bus          *event.Bus
	clock        Clock
	seedInterval time.Duration

	terminatingTimeout time.Duration
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
	timeout := o.terminatingTimeout
	if timeout <= 0 {
		timeout = terminatingTimeout
	}
	return &ProjectReschedulerController{
		logger:       logger,
		projects:     projects,
		nodes:        nodes,
		bus:          bus,
		clock:        o.clock,
		seedInterval: interval,

		terminatingTimeout: timeout,
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
			if err := c.release(ctx, log, p); err != nil {
				return Result{}, err
			}

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
	// Failed is in this list and not in the one the NotReady path uses. The
	// rescheduler never acts on a Failed Project — it is terminal for this
	// controller — but a Project can carry a clock from when it was Running
	// and be reported Failed by its agent before the node recovers. Nothing
	// else would ever take the clock off it: node.updated fires on a phase
	// change, not on every heartbeat, and the periodic resync only re-enqueues
	// NotReady nodes, so this cleanup is the last thing that will look at it.
	projects, err := c.projects.ListProjectsByNodeRef(ctx, nodeName, []v1.ProjectPhase{
		v1.ProjectPhaseScheduled,
		v1.ProjectPhaseRunning,
		v1.ProjectPhaseTerminating,
		v1.ProjectPhaseFailed,
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

// hasRescheduleClock reports whether p carries any outage condition.
func hasRescheduleClock(p *ProjectSnapshot) bool {
	for _, cond := range p.Conditions {
		if IsOutageCondition(cond.Type) {
			return true
		}
	}
	return false
}

// IsOutageCondition reports whether t is one of the conditions this controller
// writes for a single Node outage: the two reschedule clocks, the
// TakeoverRequired mark left by a refused release, and the TerminationBlocked
// mark left by an expired termination timeout. All four describe that outage
// only, so a Node that recovers before its Projects were released takes all
// four with it.
//
// TakeoverRequired is listed even though it is not a clock: a Scheduled
// Project is released on first sight with no clock at all, and without this it
// would keep reporting a takeover that is no longer needed. TerminationBlocked
// likewise: once the Node is back, its Agent resumes the teardown, and the mark
// would otherwise keep saying that nothing can.
func IsOutageCondition(t v1.ConditionType) bool {
	switch t {
	case v1.ConditionTypeNotReadyAt, v1.ConditionTypeTerminatingAt, v1.ConditionTypeTakeoverRequired,
		v1.ConditionTypeTerminationBlocked:
		return true
	default:
		return false
	}
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
//
// The timeout it governs ends in TerminationBlocked, never in Terminated. The
// node is the only party that can confirm its resources are gone, and a record
// deleted before that confirmation would leave whatever still runs there with
// nothing that knows it exists. So the Project waits, visibly, for the node to
// return — or, once it exists, for an audited force-finalize.
//
// The clock still belongs to one outage, so the mark does too: a node that
// recovers, starts a slow teardown and fails again part-way is reported blocked
// only after this outage has lasted the full timeout.
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
	if elapsed < c.terminatingTimeout {
		remaining := c.terminatingTimeout - elapsed
		log.Info("Terminating project waiting for timeout",
			zap.Duration("elapsed", elapsed),
			zap.Duration("remaining", remaining),
		)
		return true, nil
	}

	// Timeout exceeded. Report it once per outage; the periodic resync keeps
	// enqueueing the node, and re-marking on every pass would only bump the
	// Project's version.
	if _, marked := incidentClock(p.Conditions, v1.ConditionTypeTerminationBlocked, lastHeartbeat); marked {
		return false, nil
	}
	log.Warn("Termination timeout exceeded; keeping the project Terminating until its node returns",
		zap.Duration("elapsed", elapsed),
		zap.Duration("timeout", c.terminatingTimeout),
	)
	if err := c.projects.SetTerminationBlocked(ctx, p.Name, p.NodeRef, c.clock.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project disappeared before TerminationBlocked write, skipping")
			return false, nil
		}
		return false, err
	}
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

	// Grace period exceeded — release the assignment if that is authorized.
	log.Info("Running grace period exceeded, releasing project",
		zap.Duration("elapsed", elapsed),
		zap.Duration("gracePeriod", runningGracePeriod),
	)
	return false, c.release(ctx, log, p)
}

// release asks the store to give up p's assignment and logs which way the
// release gate went.
//
// A refused release is not retried here. The Project keeps its nodeRef, so the
// periodic resync of NotReady nodes lists it again and calls release again;
// the store makes the repeated TakeoverRequired mark a no-op. Requeueing would
// only spin: the authorization it waits for has to come from the Node's own
// Agent, or from features that do not exist yet.
//
// A snapshot that went stale between the list and this write is skipped: the
// Project was deleted, reassigned, or moved to a phase this controller does
// not release, and in each case there is nothing left to release.
func (c *ProjectReschedulerController) release(ctx context.Context, log *zap.Logger, p *ProjectSnapshot) error {
	log = log.With(zap.String("project", p.Name), zap.Int64("assignmentGeneration", p.AssignmentGeneration))

	released, err := c.projects.ReleaseAssignment(ctx, p)
	switch {
	case errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrStaleAssignment),
		errors.Is(err, store.ErrConflictState):
		log.Debug("Project changed before release, skipping", zap.Error(err))
		return nil
	case err != nil:
		return err
	case released:
		log.Info("Project released for rescheduling", zap.String("phase", string(p.Phase)))
	default:
		log.Warn("Release refused: the NotReady node has not confirmed this assignment stopped; project marked TakeoverRequired",
			zap.String("phase", string(p.Phase)))
	}
	return nil
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
