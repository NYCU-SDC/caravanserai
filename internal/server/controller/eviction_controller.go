package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

const (
	// evictionStartTimeout is how long the agent on the source Node has to
	// start the fresh backup a move asked for. An agent that never does —
	// one that predates planned moves, say — would otherwise hold the move,
	// and with it a slot, forever. A backup that has started has no limit
	// here: it ends on its own, in success or failure.
	//
	// Interim value pending product sign-off.
	evictionStartTimeout = 10 * time.Minute

	// evictionBackupTimeout bounds a move once its agent has been seen at
	// work: if neither a result nor the stop evidence has arrived this long
	// after, and the agent shows no sign of still working, the move is
	// abandoned. Generous on purpose — a large backup and the clean-up after
	// it may take hours — since it only catches an agent that died mid-move.
	// Abandoning is safe whenever it lands: evidence that arrives later is
	// refused, and evidence already in keeps the move going.
	//
	// Interim value pending product sign-off.
	evictionBackupTimeout = 6 * time.Hour

	// evictionPlaceTimeout is how long a moved Project may wait in Pending
	// for the scheduler before the move is declared stuck and its slot is
	// freed. The Project stays Pending and the scheduler keeps trying.
	//
	// Interim value pending product sign-off.
	evictionPlaceTimeout = 10 * time.Minute

	// evictionResyncInterval is how often the Seed loop re-enqueues the queue
	// key, for the timeouts no event announces.
	evictionResyncInterval = 30 * time.Second

	// evictionQueueKey is the only key this controller reconciles. The limit
	// on moves in flight is cluster-wide, so every move is decided in one
	// pass over all of them rather than one Project at a time.
	evictionQueueKey = "evictions"

	// DefaultMaxInFlightEvictions is how many planned moves run at once when
	// the server is not configured otherwise. One at a time: every move is
	// downtime plus a backup and a restore, and with one in flight each
	// capacity precheck sees the previous move already placed.
	DefaultMaxInFlightEvictions = 1
)

// EvictionRequester is how a trigger — drain or fail-back — asks for a planned
// move. The move itself, and choosing where the Project goes, is not the
// trigger's job.
type EvictionRequester interface {
	// RequestEviction queues a move of the named Project off fromNode, with
	// reason saying who asked. targetTier, when not empty, is the tier the
	// Project must land in; see v1.ProjectEviction.TargetTier. It fails with
	// store.ErrConflictState when the Project is not Running on fromNode, and
	// does nothing when a move is already in progress.
	RequestEviction(ctx context.Context, name, fromNode, reason string, targetTier v1.NodeTier) error
}

// EvictionSnapshot is the view of a Project with a planned move in progress.
type EvictionSnapshot struct {
	Name                 string
	UID                  string
	Phase                v1.ProjectPhase
	NodeRef              string
	AssignmentGeneration int64
	Size                 v1.ProjectSize
	FailedNodes          []string

	// WorkloadStopped reports whether the Agent of the current assignment
	// has confirmed the workload stopped, which is what lets the move release
	// the assignment.
	WorkloadStopped bool

	// HasManagedVolumes reports whether the Project has data the move must
	// carry, which is what the fresh backup is for.
	HasManagedVolumes bool

	// HasPostgresDifferential reports whether a Managed volume is backed up
	// with PostgresDifferential, which a move cannot take a fresh backup of.
	HasPostgresDifferential bool

	Eviction   v1.ProjectEviction
	Conditions []v1.Condition
}

// EvictionFailure says why a move was abandoned.
type EvictionFailure struct {
	Reason  string
	Message string
}

// Reasons an EvictionFailed condition can carry.
const (
	EvictionFailedNoCapacity       = "NoCapacity"
	EvictionFailedBackupFailed     = "BackupFailed"
	EvictionFailedAgentDidNotStart = "AgentDidNotStart"
	// EvictionFailedAgentDidNotFinish: the agent started the backup and then
	// went silent for evictionBackupTimeout.
	EvictionFailedAgentDidNotFinish = "AgentDidNotFinish"
	EvictionFailedNotPlaced         = "NotPlaced"

	// EvictionFailedUnsupportedBackupStrategy: the Project has a
	// PostgresDifferential volume. A move takes an offline backup, and moving
	// without a fresh one would lose what was written since the last.
	EvictionFailedUnsupportedBackupStrategy = "UnsupportedBackupStrategy"
)

// EvictionStore is the store surface needed by EvictionController.
type EvictionStore interface {
	// CapacityProjectStore lets the capacity precheck see what is placed on
	// each Node.
	CapacityProjectStore

	// ListEvictions returns every Project with a move in progress, oldest
	// request first.
	ListEvictions(ctx context.Context) ([]*EvictionSnapshot, error)

	// StartEvictionBackup asks the agent on fromNode for the fresh backup. It
	// does nothing unless the move is still Queued and the Project still
	// Running on fromNode.
	StartEvictionBackup(ctx context.Context, name, fromNode string) error

	// MoveEvictedProject releases the assignment the snapshot holds, returning
	// the Project to Pending, with message as the reason shown. It reports
	// whether it did: it does nothing unless the Project is still Running on
	// the source Node under the same generation and its Agent has confirmed the
	// workload stopped.
	MoveEvictedProject(ctx context.Context, e *EvictionSnapshot, message string) (bool, error)

	// EndEviction clears the move; a non-nil failure records why it was
	// abandoned. It writes nothing for another lifetime than the snapshot's,
	// and refuses to abandon the move with store.ErrConflictState once the
	// Agent's stop evidence for the current assignment is in.
	EndEviction(ctx context.Context, e *EvictionSnapshot, failure *EvictionFailure) error

	// MarkEvictionAgentStarted records when the agent was first seen at work
	// on the move's backup. It does nothing if the move changed since the
	// snapshot or is already marked.
	MarkEvictionAgentStarted(ctx context.Context, e *EvictionSnapshot, at time.Time) error
}

// EvictionController carries out planned moves: a Project moved off a healthy
// Node because someone wants it elsewhere, not because it failed. Drain
// (CARA-120) and fail-back (CARA-121) ask for moves through EvictionRequester;
// this controller is the one mechanism both share (docs/scheduler-strategy.md
// §5).
//
// A move goes Queued -> BackingUp -> Moving -> done:
//
//   - Queued: waits until fewer than maxInFlight moves are running, oldest
//     first. Before it starts, a capacity precheck asks whether any Node other
//     than the source could take the Project; if none can, the move is
//     abandoned and the Project is left running where it is.
//   - BackingUp: the agent on the source Node stops the Project, takes a fresh
//     backup and leaves it stopped, so the new Node restores everything
//     written up to the move. A Project without Managed volumes has nothing
//     to carry and skips this.
//   - Moving: the Project is back in Pending and the scheduler places it. The
//     source Node is excluded; otherwise placement is the scheduler's alone.
//     The move is done when the Project is Scheduled on its new Node, and
//     only then is its slot freed, so the next precheck sees it placed.
//
// A move whose Project leaves the source Node on its own — failover, a delete,
// a failure — is dropped quietly: whatever moved it is in charge now. Every
// other way a move can end short is an alert.
type EvictionController struct {
	logger       *zap.Logger
	projects     EvictionStore
	nodes        SchedulerNodeStore
	ledger       *CapacityLedger
	bus          *event.Bus
	clock        Clock
	maxInFlight  int
	seedInterval time.Duration
}

// NewEvictionController creates an EvictionController. maxInFlight below one
// is taken as one. bus may be nil; if so the controller relies solely on the
// periodic resync.
func NewEvictionController(
	logger *zap.Logger,
	projects EvictionStore,
	nodes SchedulerNodeStore,
	bus *event.Bus,
	maxInFlight int,
	opts ...Option,
) *EvictionController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = evictionResyncInterval
	}
	return &EvictionController{
		logger:       logger,
		projects:     projects,
		nodes:        nodes,
		ledger:       NewCapacityLedger(logger, projects),
		bus:          bus,
		clock:        o.clock,
		maxInFlight:  max(maxInFlight, 1),
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *EvictionController) Name() string { return "project-evictor" }

// Reconcile implements Controller. The key is always evictionQueueKey; every
// move is advanced in one pass.
func (c *EvictionController) Reconcile(ctx context.Context, _ string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()))

	evictions, err := c.projects.ListEvictions(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(evictions) == 0 {
		return Result{}, nil
	}

	inFlight := 0
	var queued []*EvictionSnapshot
	for _, e := range evictions {
		elog := log.With(zap.String("project", e.Name),
			zap.String("fromNode", e.Eviction.FromNode), zap.String("reason", e.Eviction.Reason))

		var busy bool
		switch e.Eviction.State {
		case v1.EvictionStateQueued:
			if !onSourceNode(e) {
				err = c.abandonQuietly(ctx, elog, e)
			} else {
				queued = append(queued, e)
			}
		case v1.EvictionStateBackingUp:
			busy, err = c.reconcileBackingUp(ctx, elog, e)
		case v1.EvictionStateMoving:
			busy, err = c.reconcileMoving(ctx, elog, e)
		default:
			err = c.abandonQuietly(ctx, elog, e)
		}
		if err != nil {
			return Result{}, err
		}
		if busy {
			inFlight++
		}
	}

	for _, e := range queued {
		if inFlight >= c.maxInFlight {
			log.Debug("Moves in flight at the limit, the rest stay queued",
				zap.Int("inFlight", inFlight), zap.Int("maxInFlight", c.maxInFlight))
			break
		}
		elog := log.With(zap.String("project", e.Name),
			zap.String("fromNode", e.Eviction.FromNode), zap.String("reason", e.Eviction.Reason))
		started, err := c.start(ctx, elog, e)
		if err != nil {
			return Result{}, err
		}
		if started {
			inFlight++
		}
	}

	// Moves are still running or waiting; their timeouts have no event.
	return Result{Requeue: true}, nil
}

// start runs the capacity precheck for a queued move and, if some other Node
// could take the Project, starts it. It reports whether the move now holds a
// slot.
func (c *EvictionController) start(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) (bool, error) {
	if e.HasPostgresDifferential {
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedUnsupportedBackupStrategy,
			"A planned move cannot yet take a fresh backup of a PostgresDifferential volume; the project stays where it is")
	}

	nodes, err := c.nodes.ListReadyNodes(ctx)
	if err != nil {
		return false, err
	}
	candidates, rejected, err := filterNodes(ctx, c.ledger, nodes, ProjectPlacement{
		Size:          e.Size,
		ExcludedNodes: append([]string{e.Eviction.FromNode}, e.FailedNodes...),
		RequiredTier:  e.Eviction.TargetTier,
	})
	if err != nil {
		return false, err
	}
	if len(candidates) == 0 {
		message := "No other node can take the project; it stays where it is"
		if tier := e.Eviction.TargetTier; tier != "" {
			message = fmt.Sprintf("No other %s node can take the project; it stays where it is", tier)
		}
		if len(rejected) > 0 {
			message += ": " + strings.Join(rejected, "; ")
		}
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedNoCapacity, message)
	}

	// Every Project is stopped by its Agent before it is released, data or
	// not: the release gate requires the Agent's word that the workload
	// stopped, so the old copy cannot run beside the new one. A Project with
	// Managed data is also backed up while it is stopped.
	log.Info("Starting planned move: asking the agent to stop the project",
		zap.Bool("freshBackup", e.HasManagedVolumes))
	return true, ignoreNotFound(c.projects.StartEvictionBackup(ctx, e.Name, e.Eviction.FromNode))
}

// reconcileBackingUp waits for the Agent to stop the Project — and to commit
// the fresh backup, when it has Managed data — and releases it once the Agent
// has confirmed the workload stopped. It reports whether the move still holds
// a slot.
func (c *EvictionController) reconcileBackingUp(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) (bool, error) {
	if !onSourceNode(e) {
		return false, c.abandonQuietly(ctx, log, e)
	}

	result, reported := evictionBackupResult(e)
	if reported && result.Status != v1.ConditionTrue && !e.WorkloadStopped {
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedBackupFailed,
			"The project could not be stopped and backed up for the move, and was started again where it is: "+result.Message)
	}

	// The stop evidence is what the release rests on. Once it is recorded the
	// Agent will not start this generation again, so the move must go on
	// even if its own report was lost.
	if e.WorkloadStopped {
		how := "it has no Managed volume"
		if e.HasManagedVolumes {
			how = "restored from its fresh backup"
			// Only a successful result carries the backup's ID; a failure's
			// message is an error, not something to restore from.
			if reported && result.Status == v1.ConditionTrue && result.Message != "" {
				how = "restored from backup " + result.Message
			}
		}
		moved, err := c.projects.MoveEvictedProject(ctx, e, moveMessage(e, how))
		if err != nil {
			return true, ignoreNotFound(err)
		}
		if moved {
			log.Info("Project stopped on its node; released for the scheduler to place elsewhere")
		}
		return true, nil
	}

	// Maintenance is how the agent shows it is at work, but it is cleared
	// before the agent has finished — old backups are pruned and the
	// evidence sent after — and is ignored once it is older than
	// MaintenanceStaleAfter. So once seen it is remembered on the move, and
	// the start timeout no longer applies.
	working := v1.IsMaintenanceActive(e.Conditions, c.clock.Now()) || reported
	startedAt := e.Eviction.AgentStartedAt
	if working && startedAt == nil {
		now := c.clock.Now()
		if err := c.projects.MarkEvictionAgentStarted(ctx, e, now); err != nil {
			log.Warn("Could not record that the agent started the move's backup", zap.Error(err))
		} else {
			startedAt = &now
		}
	}

	switch {
	case startedAt == nil && c.clock.Since(e.Eviction.StateSince) >= evictionStartTimeout:
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedAgentDidNotStart,
			fmt.Sprintf("The agent on node %q did not start the fresh backup within %s", e.Eviction.FromNode, evictionStartTimeout))
	case startedAt != nil && !working && c.clock.Since(*startedAt) >= evictionBackupTimeout:
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedAgentDidNotFinish,
			fmt.Sprintf("The agent on node %q started the fresh backup but has not finished it within %s", e.Eviction.FromNode, evictionBackupTimeout))
	}
	return true, nil
}

// reconcileMoving waits for the scheduler to place the moved Project. It
// reports whether the move still holds a slot.
func (c *EvictionController) reconcileMoving(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) (bool, error) {
	switch {
	case e.NodeRef != "":
		log.Info("Planned move complete", zap.String("toNode", e.NodeRef))
		return false, ignoreNotFound(c.projects.EndEviction(ctx, e, nil))

	case e.Phase != v1.ProjectPhasePending:
		// Deleted or failed on the way; nothing left to finish.
		return false, c.abandonQuietly(ctx, log, e)

	case c.clock.Since(e.Eviction.StateSince) >= evictionPlaceTimeout:
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedNotPlaced,
			fmt.Sprintf("The project left node %q but no node has taken it within %s; it stays Pending and the scheduler keeps trying",
				e.Eviction.FromNode, evictionPlaceTimeout))
	}
	return true, nil
}

// abandonQuietly drops a move whose Project is no longer where the move
// expects it. Something else moved or stopped it, which is not a failure of
// the move.
func (c *EvictionController) abandonQuietly(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) error {
	log.Info("Project is no longer on the node it was being moved off; dropping the move",
		zap.String("phase", string(e.Phase)), zap.String("nodeRef", e.NodeRef))
	return ignoreNotFound(c.projects.EndEviction(ctx, e, nil))
}

// raiseEvictionFailedAlert is the one place an abandoned move is announced. It
// writes the EvictionFailed condition, which `caractl describe project` shows,
// and logs at Error level, which a log-based alert rule can match.
//
// TODO(discord): post here too once Discord notifications exist; see the
// TODO(discord) on raiseRescheduleExhaustedAlert for what to handle.
func (c *EvictionController) raiseEvictionFailedAlert(ctx context.Context, log *zap.Logger, e *EvictionSnapshot, reason, message string) error {
	err := c.projects.EndEviction(ctx, e, &EvictionFailure{Reason: reason, Message: message})
	if errors.Is(err, store.ErrConflictState) {
		// The stop evidence arrived after the read this decision was made
		// on. The move goes on; the next pass releases the Project.
		log.Info("Not abandoning the move: the agent has reported the project stopped", zap.String("failure", reason))
		return nil
	}
	if err == nil {
		log.Error("Planned move abandoned", zap.String("failure", reason), zap.String("detail", message))
	}
	return ignoreNotFound(err)
}

// onSourceNode reports whether the Project is still running on the Node the
// move is taking it off.
func onSourceNode(e *EvictionSnapshot) bool {
	return e.Phase == v1.ProjectPhaseRunning && e.NodeRef == e.Eviction.FromNode
}

// evictionBackupResult returns the agent's result for this move's backup. A
// result stamped before the move entered BackingUp belongs to an earlier move
// and is ignored.
func evictionBackupResult(e *EvictionSnapshot) (v1.Condition, bool) {
	for _, c := range e.Conditions {
		if c.Type == v1.ConditionTypeEvictionBackup && !c.LastTransitionTime.Before(e.Eviction.StateSince) {
			return c, true
		}
	}
	return v1.Condition{}, false
}

func moveMessage(e *EvictionSnapshot, how string) string {
	return fmt.Sprintf("Moved off node %q for %s; %s", e.Eviction.FromNode, e.Eviction.Reason, how)
}

func ignoreNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// Seed implements controller.Seeder. Any Project update may advance a move —
// an agent reporting its backup, the scheduler placing a Project — so each
// one enqueues the queue key; the work queue collapses repeats. The resync
// ticker covers the timeouts.
func (c *EvictionController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var updated event.Handler
	if c.bus != nil {
		updated = c.bus.Subscribe(event.TopicProjectUpdated)
		log.Debug("Subscribed to project.updated events")
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	enqueue(evictionQueueKey)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-updated:
			if !ok {
				updated = nil
				continue
			}
			enqueue(evictionQueueKey)
		case <-tick.C:
			enqueue(evictionQueueKey)
		}
	}
}
