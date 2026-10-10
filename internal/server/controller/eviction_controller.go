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
	Name        string
	Phase       v1.ProjectPhase
	NodeRef     string
	Size        v1.ProjectSize
	FailedNodes []string

	// HasManagedVolumes reports whether the Project has data the move must
	// carry, which is what the fresh backup is for.
	HasManagedVolumes bool

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
	EvictionFailedNotPlaced        = "NotPlaced"
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

	// MoveEvictedProject returns the Project to Pending off fromNode, with
	// message as the reason shown. It does nothing unless the Project is
	// still Running on fromNode.
	MoveEvictedProject(ctx context.Context, name, fromNode, message string) error

	// EndEviction clears the move; a non-nil failure records why it was
	// abandoned.
	EndEviction(ctx context.Context, name string, failure *EvictionFailure) error
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

	if !e.HasManagedVolumes {
		log.Info("Moving project with no data to carry")
		return true, ignoreNotFound(c.projects.MoveEvictedProject(ctx, e.Name, e.Eviction.FromNode, moveMessage(e, "it has no Managed volume")))
	}

	log.Info("Starting planned move: asking the agent for a fresh backup")
	return true, ignoreNotFound(c.projects.StartEvictionBackup(ctx, e.Name, e.Eviction.FromNode))
}

// reconcileBackingUp waits for the agent's backup result and moves the Project
// once the backup is committed. It reports whether the move still holds a slot.
func (c *EvictionController) reconcileBackingUp(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) (bool, error) {
	if !onSourceNode(e) {
		return false, c.abandonQuietly(ctx, log, e)
	}

	if result, ok := evictionBackupResult(e); ok {
		if result.Status == v1.ConditionTrue {
			log.Info("Fresh backup committed, returning project to Pending", zap.String("backup", result.Message))
			err := c.projects.MoveEvictedProject(ctx, e.Name, e.Eviction.FromNode, moveMessage(e, "restored from backup "+result.Message))
			return true, ignoreNotFound(err)
		}
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedBackupFailed,
			"The fresh backup failed and the project was started again where it is: "+result.Message)
	}

	started := v1.IsMaintenanceActive(e.Conditions, c.clock.Now())
	if waited := c.clock.Since(e.Eviction.StateSince); !started && waited >= evictionStartTimeout {
		return false, c.raiseEvictionFailedAlert(ctx, log, e, EvictionFailedAgentDidNotStart,
			fmt.Sprintf("The agent on node %q did not start the fresh backup within %s", e.Eviction.FromNode, evictionStartTimeout))
	}
	return true, nil
}

// reconcileMoving waits for the scheduler to place the moved Project. It
// reports whether the move still holds a slot.
func (c *EvictionController) reconcileMoving(ctx context.Context, log *zap.Logger, e *EvictionSnapshot) (bool, error) {
	switch {
	case e.NodeRef != "":
		log.Info("Planned move complete", zap.String("toNode", e.NodeRef))
		return false, ignoreNotFound(c.projects.EndEviction(ctx, e.Name, nil))

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
	return ignoreNotFound(c.projects.EndEviction(ctx, e.Name, nil))
}

// raiseEvictionFailedAlert is the one place an abandoned move is announced. It
// writes the EvictionFailed condition, which `caractl describe project` shows,
// and logs at Error level, which a log-based alert rule can match.
//
// TODO(discord): post here too once Discord notifications exist; see the
// TODO(discord) on raiseRescheduleExhaustedAlert for what to handle.
func (c *EvictionController) raiseEvictionFailedAlert(ctx context.Context, log *zap.Logger, e *EvictionSnapshot, reason, message string) error {
	log.Error("Planned move abandoned", zap.String("failure", reason), zap.String("detail", message))
	return ignoreNotFound(c.projects.EndEviction(ctx, e.Name, &EvictionFailure{Reason: reason, Message: message}))
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
