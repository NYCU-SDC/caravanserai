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

// rescheduleBackoff is how long a Project must have been Failed before it is
// moved, indexed by the moves already spent: the first move waits a minute, the
// second five. Tier-1 in-place recovery has already spent about fifteen seconds
// by the time a Project is Failed, so the first wait is short — a Node-level
// fault is most likely and a different Node is the fix — while the second
// follows a move that did not help and gives a person time to notice.
//
// Interim values pending product sign-off (docs/scheduler-strategy.md §3.5).
var rescheduleBackoff = [...]time.Duration{1 * time.Minute, 5 * time.Minute}

// maxReschedules is how many times a Failed Project is moved to a different
// Node before it is left Failed. It is the length of rescheduleBackoff, so the
// two cannot disagree.
const maxReschedules = len(rescheduleBackoff)

// rescheduleStableAfter is how long a moved Project must keep running on its
// new Node before the move counts as having worked and its history is
// cleared. Without it, a service that crashes a little while after every start
// would be moved forever; with it, such a service is moved only as often as
// this interval allows.
//
// Interim value pending product sign-off.
const rescheduleStableAfter = 3 * time.Hour

// failedReschedulerResyncInterval is how often the Seed loop re-enqueues the
// Projects this controller watches, as a fallback for dropped events and for
// the clocks no event announces.
const failedReschedulerResyncInterval = 30 * time.Second

// rescheduleReasons are the Failed reasons worth moving a Project for: the
// ones a different Node can fix, or may. Everything else is left Failed.
//
// An allowlist, so a reason added later is not moved until someone decides it
// should be. The ones left out stay out for a reason:
//
//   - RemoveError is a failure to delete a Project the user asked to delete;
//     moving it would bring it back.
//   - SecretNotFound and SecretKeyNotFound are a problem with the Project's own
//     configuration, which every Node has.
//   - RestoreError may be the object store, which every Node shares.
//   - ContainerUnhealthy covers a container that something outside cara paused
//     or left in a state cara does not recognise, which needs a person.
//   - ContainerMissing is not yet judged worth a move.
var rescheduleReasons = map[string]bool{
	"LocalRestartExhausted": true,
	"ContainerRestartStuck": true,
	"InspectError":          true,
	"ReconcileError":        true,
	"ContainerExited":       true,
	"ContainerCrashed":      true,
}

// phaseReasonRunning is the Reason an agent reports with Running.
const phaseReasonRunning = "ContainersRunning"

// FailedProjectStore is the store surface needed by
// FailedProjectReschedulerController.
type FailedProjectStore interface {
	// ListProjectNamesByPhase returns the names of all Projects in the given phase.
	ListProjectNamesByPhase(ctx context.Context, phase v1.ProjectPhase) ([]string, error)

	// GetProjectSnapshot returns the named Project.
	GetProjectSnapshot(ctx context.Context, name string) (*ProjectSnapshot, error)

	// MoveFailedProject returns a Failed Project to Pending, clearing its
	// nodeRef and adding fromNode to its FailedNodes, with message as the
	// reason shown for the move. It does nothing unless the Project is still
	// Failed on fromNode.
	MoveFailedProject(ctx context.Context, name, fromNode, message string) error

	// ClearFailedNodes empties the Project's FailedNodes.
	ClearFailedNodes(ctx context.Context, name string) error

	// SetRescheduleExhausted marks a Failed Project as out of moves, with
	// message saying where it failed.
	SetRescheduleExhausted(ctx context.Context, name, message string) error
}

// FailedProjectReschedulerController is Tier-2 recovery (docs/scheduler-strategy.md
// §3.5). The agent's Tier-1 recovery restarts a failed Project in place; when
// that is used up the agent reports Failed, and nothing else would ever act.
// This controller then moves the Project to a different Node, a bounded number
// of times, and leaves it Failed with an alert when the moves run out.
//
// A move returns the Project to Pending and records the Node in FailedNodes;
// the scheduler places it again, never on a Node in that list. A Project that
// has a Managed volume is restored on the new Node from the newest complete
// backup, since there is no local data there — the crashed Node's state is not
// carried over, and a Failed Project is not backed up, so that backup predates
// the failure.
//
// It is keyed by Project, unlike ProjectReschedulerController, which is keyed
// by Node and handles a Node going NotReady.
type FailedProjectReschedulerController struct {
	logger       *zap.Logger
	projects     FailedProjectStore
	bus          *event.Bus
	clock        Clock
	seedInterval time.Duration
}

// NewFailedProjectReschedulerController creates a
// FailedProjectReschedulerController. bus may be nil; if so the controller
// relies solely on the periodic resync.
func NewFailedProjectReschedulerController(
	logger *zap.Logger,
	projects FailedProjectStore,
	bus *event.Bus,
	opts ...Option,
) *FailedProjectReschedulerController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = failedReschedulerResyncInterval
	}
	return &FailedProjectReschedulerController{
		logger:       logger,
		projects:     projects,
		bus:          bus,
		clock:        o.clock,
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *FailedProjectReschedulerController) Name() string { return "failed-project-rescheduler" }

// Reconcile implements Controller.
//
// name is the name of a Project that may have failed, or that was moved and
// may by now have recovered.
func (c *FailedProjectReschedulerController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("project", name))

	p, err := c.projects.GetProjectSnapshot(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project not found, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}

	switch p.Phase {
	case v1.ProjectPhaseFailed:
		return c.reconcileFailed(ctx, log, p)
	case v1.ProjectPhaseRunning:
		return Result{}, c.reconcileRunning(ctx, log, p)
	default:
		return Result{}, nil
	}
}

// reconcileFailed moves a Failed Project once it has waited out its backoff,
// or raises the alert when it has no moves left.
func (c *FailedProjectReschedulerController) reconcileFailed(ctx context.Context, log *zap.Logger, p *ProjectSnapshot) (Result, error) {
	phase, ok := phaseCondition(p)
	if !ok || !rescheduleReasons[phase.Reason] {
		log.Debug("Failed for a reason that a new Node does not fix, leaving it",
			zap.String("reason", phase.Reason))
		return Result{}, nil
	}
	if p.NodeRef == "" {
		return Result{}, nil
	}

	spent := len(p.FailedNodes)
	if spent >= maxReschedules {
		if hasCondition(p, v1.ConditionTypeRescheduleExhausted) {
			return Result{}, nil
		}
		return Result{}, c.raiseRescheduleExhaustedAlert(ctx, log, p, phase.Reason)
	}

	wait := rescheduleBackoff[spent]
	if failedFor := c.clock.Since(phase.LastTransitionTime); failedFor < wait {
		log.Debug("Failed project waiting out its backoff",
			zap.Duration("failedFor", failedFor), zap.Duration("backoff", wait))
		return Result{Requeue: true}, nil
	}

	message := fmt.Sprintf("Failed on node %q (%s); moved to a different node (move %d of %d)",
		p.NodeRef, phase.Reason, spent+1, maxReschedules)
	log.Warn("Moving failed project to a different node",
		zap.String("fromNode", p.NodeRef),
		zap.String("reason", phase.Reason),
		zap.Int("move", spent+1),
		zap.Int("maxMoves", maxReschedules))

	if err := c.projects.MoveFailedProject(ctx, p.Name, p.NodeRef, message); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Project disappeared before the move, skipping")
			return Result{}, nil
		}
		return Result{}, err
	}
	return Result{}, nil
}

// reconcileRunning clears the history of a moved Project that has run long
// enough to count as recovered. It does not requeue: the periodic resync looks
// at every Running Project, and the time left is hours.
func (c *FailedProjectReschedulerController) reconcileRunning(ctx context.Context, log *zap.Logger, p *ProjectSnapshot) error {
	if len(p.FailedNodes) == 0 {
		return nil
	}
	phase, ok := phaseCondition(p)
	if !ok || phase.Reason != phaseReasonRunning {
		// Without the time it began running, it cannot be judged stable.
		return nil
	}
	if runningFor := c.clock.Since(phase.LastTransitionTime); runningFor < rescheduleStableAfter {
		log.Debug("Moved project not yet stable",
			zap.Duration("runningFor", runningFor), zap.Duration("stableAfter", rescheduleStableAfter))
		return nil
	}

	log.Info("Moved project has been stable, clearing its failed nodes",
		zap.Strings("failedNodes", p.FailedNodes))
	if err := c.projects.ClearFailedNodes(ctx, p.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// raiseRescheduleExhaustedAlert is the one place a Project that has run out of
// moves is announced. It writes the RescheduleExhausted condition, which
// `caractl describe project` shows, and logs at Error level, which a log-based
// alert rule can match.
//
// TODO(discord): also post to a Discord webhook from here, so an operator is
// told rather than having to look. When doing so:
//   - Read the webhook URL from configuration (env var or config file). It is
//     a secret — anyone holding it can post to the channel — so keep it out of
//     the repo and out of logs.
//   - Treat the post as best effort: use an HTTP client with a timeout, run it
//     off the reconcile path, and only log a failure. Discord being down must
//     not delay or fail scheduling.
//   - Keep the message short (Discord caps a message at 2000 characters and
//     rate-limits a webhook to about 30 posts a minute): the project, the
//     nodes it failed on, and the reason.
//   - The caller already ensures this runs once per exhaustion, since the
//     condition written here is checked before it is called; do not add a
//     second source of repeats.
//   - The server needs outbound internet access to discord.com.
func (c *FailedProjectReschedulerController) raiseRescheduleExhaustedAlert(ctx context.Context, log *zap.Logger, p *ProjectSnapshot, reason string) error {
	failed := append(append([]string{}, p.FailedNodes...), p.NodeRef)
	message := fmt.Sprintf("Failed (%s) on every node it was tried on: %s; moved %d times, no moves left",
		reason, strings.Join(failed, ", "), maxReschedules)

	log.Error("Failed project has used all its moves and needs an operator",
		zap.String("reason", reason),
		zap.Strings("failedNodes", failed),
		zap.Int("maxMoves", maxReschedules))

	if err := c.projects.SetRescheduleExhausted(ctx, p.Name, message); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// phaseCondition returns the Project's Phase condition, which carries why it is
// in its phase and since when.
func phaseCondition(p *ProjectSnapshot) (ConditionSnapshot, bool) {
	for _, c := range p.Conditions {
		if c.Type == v1.ConditionTypePhase {
			return c, true
		}
	}
	return ConditionSnapshot{}, false
}

// hasCondition reports whether the Project carries a condition of type t.
func hasCondition(p *ProjectSnapshot, t v1.ConditionType) bool {
	for _, c := range p.Conditions {
		if c.Type == t {
			return true
		}
	}
	return false
}

// Seed implements controller.Seeder.
//
// It has two sources of work:
//  1. Event bus: project.updated, so a Project reported Failed is looked at
//     straight away (and waits out its backoff from there).
//  2. Resync ticker: every failedReschedulerResyncInterval it lists the Failed
//     Projects and the Running ones, the latter being how a moved Project's
//     stability is noticed, since an unchanged Running Project publishes nothing.
func (c *FailedProjectReschedulerController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var updated event.Handler
	if c.bus != nil {
		updated = c.bus.Subscribe(event.TopicProjectUpdated)
		log.Debug("Subscribed to project.updated events")
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	c.resync(ctx, log, enqueue)

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-updated:
			if !ok {
				updated = nil
				continue
			}
			enqueue(e.Name)

		case <-tick.C:
			c.resync(ctx, log, enqueue)
		}
	}
}

func (c *FailedProjectReschedulerController) resync(ctx context.Context, log *zap.Logger, enqueue func(name string)) {
	for _, phase := range []v1.ProjectPhase{v1.ProjectPhaseFailed, v1.ProjectPhaseRunning} {
		names, err := c.projects.ListProjectNamesByPhase(ctx, phase)
		if err != nil {
			log.Warn("Failed to list projects for resync", zap.String("phase", string(phase)), zap.Error(err))
			continue
		}
		for _, n := range names {
			enqueue(n)
		}
	}
}
