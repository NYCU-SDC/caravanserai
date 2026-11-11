package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

const (
	// DrainReason is the Reason on a planned move a drain asks for.
	DrainReason = "Drain"

	// drainRetryAfter is how long a drain waits before asking again for a
	// move that failed for a reason time can fix: no room yet, or an Agent
	// that did not start. Interim value pending product sign-off.
	drainRetryAfter = 5 * time.Minute

	// drainResyncInterval is how often every draining Node is looked at
	// again, for the retries no event announces.
	drainResyncInterval = time.Minute
)

// drainRetryable are the planned-move failures a drain asks again for by
// itself. Each costs nothing to retry: the Project was never stopped, or the
// move has already left the Node. The others — a failed backup or stop, an
// unsupported backup strategy — would stop the Project again for nothing, or
// can never succeed, so they wait for an operator.
var drainRetryable = map[string]bool{
	EvictionFailedNoCapacity:       true,
	EvictionFailedAgentDidNotStart: true,
	EvictionFailedNotPlaced:        true,
}

// NodeDrainSnapshot is the view of a Node the drain controller needs.
type NodeDrainSnapshot struct {
	Name string
	// Drain is the operator's request (Spec.Drain).
	Drain bool
	// Status is the progress last recorded, nil if none.
	Status *v1.NodeDrainStatus
}

// DrainProjectSnapshot is the view of a Project on a draining Node.
type DrainProjectSnapshot struct {
	Name     string
	Phase    v1.ProjectPhase
	Eviction *v1.ProjectEviction
	// EvictionFailed is the last abandoned planned move, if any.
	EvictionFailed *v1.Condition
}

// DrainStore is the store surface needed by DrainController.
type DrainStore interface {
	// EvictionRequester asks for the moves.
	EvictionRequester

	// ListNodeDrains returns the Nodes being drained, and those still
	// carrying the record of a drain that was called off.
	ListNodeDrains(ctx context.Context) ([]string, error)

	// GetNodeDrain returns the named Node's drain request and progress.
	GetNodeDrain(ctx context.Context, name string) (NodeDrainSnapshot, error)

	// ListProjectsOnNode returns every Project assigned to the Node.
	ListProjectsOnNode(ctx context.Context, node string) ([]DrainProjectSnapshot, error)

	// ListProjectsOwedCleanup names the Projects whose old containers or
	// local Managed data the Node has yet to clean up.
	ListProjectsOwedCleanup(ctx context.Context, node string) ([]string, error)

	// SetNodeDrainStatus records the drain's progress; nil clears it.
	SetNodeDrainStatus(ctx context.Context, name string, status *v1.NodeDrainStatus) error

	// CancelEviction calls off a planned move while that is still safe, and
	// reports whether it did.
	CancelEviction(ctx context.Context, name, fromNode, reason string) (bool, error)
}

// DrainController empties a Node an operator wants to power off (CARA-120,
// docs/scheduler-strategy.md §3.6). Setting Spec.Drain asks for it;
// `caractl node drain` also cordons the Node, so nothing new arrives.
//
// Every Running Project on the Node is moved with a planned move (CARA-119):
// stopped, backed up if it has Managed data, released, and placed elsewhere
// by the scheduler. A move that fails for a reason time can fix is asked for
// again after drainRetryAfter; any other leaves the Project where it is for an
// operator. Progress is in Status.Drain: Draining while Projects are leaving,
// Incomplete when one cannot leave without help, Drained once nothing is
// assigned to the Node and it owes no cleanup — the point at which powering
// it off loses nothing.
//
// Clearing Spec.Drain (`caractl node uncordon`) calls the drain off: moves it
// asked for are cancelled where that is still safe and left to finish where
// it is not, and Status.Drain is cleared.
//
// The Node's health is not the drain's business: a draining Node that fails
// goes NotReady, and failover moves what it can, as for any other Node.
type DrainController struct {
	logger       *zap.Logger
	store        DrainStore
	bus          *event.Bus
	clock        Clock
	seedInterval time.Duration
}

// NewDrainController creates a DrainController. bus may be nil; if so the
// controller relies solely on the periodic resync.
func NewDrainController(logger *zap.Logger, s DrainStore, bus *event.Bus, opts ...Option) *DrainController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = drainResyncInterval
	}
	return &DrainController{logger: logger, store: s, bus: bus, clock: o.clock, seedInterval: interval}
}

// Name implements Controller.
func (c *DrainController) Name() string { return "node-drainer" }

// Reconcile implements Controller. name is a Node.
func (c *DrainController) Reconcile(ctx context.Context, name string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("node", name))

	node, err := c.store.GetNodeDrain(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, err
	}

	projects, err := c.store.ListProjectsOnNode(ctx, name)
	if err != nil {
		return Result{}, err
	}

	if !node.Drain {
		return Result{}, c.callOff(ctx, log, node, projects)
	}

	var blocking []string
	waiting := 0
	now := c.clock.Now()
	for _, p := range projects {
		switch {
		case p.Phase == v1.ProjectPhaseFailed:
			blocking = append(blocking, fmt.Sprintf(
				"%s: Failed here; delete it, or take it over with `caractl takeover %s`, to move it", p.Name, p.Name))

		case p.Phase == v1.ProjectPhaseRunning && p.Eviction == nil:
			if f := p.EvictionFailed; f != nil {
				retryIn := drainRetryAfter - now.Sub(f.LastTransitionTime)
				if !drainRetryable[f.Reason] {
					blocking = append(blocking, fmt.Sprintf("%s: cannot be moved (%s): %s", p.Name, f.Reason, f.Message))
					continue
				}
				if retryIn > 0 {
					blocking = append(blocking, fmt.Sprintf("%s: move failed (%s), retrying in %s: %s",
						p.Name, f.Reason, retryIn.Round(time.Second), f.Message))
					continue
				}
			}
			if err := c.requestMove(ctx, log, p.Name, name); err != nil {
				return Result{}, err
			}
			waiting++

		default:
			// Scheduled (will be moved once Running), Terminating, already
			// moving, or released and waiting: all leave on their own.
			waiting++
		}
	}

	owed, err := c.store.ListProjectsOwedCleanup(ctx, name)
	if err != nil {
		return Result{}, err
	}

	status := &v1.NodeDrainStatus{Remaining: len(projects) + len(owedNotAssigned(owed, projects)), Blocking: blocking}
	switch {
	case len(blocking) > 0:
		status.Phase = v1.NodeDrainIncomplete
	case waiting > 0 || len(owed) > 0:
		status.Phase = v1.NodeDrainDraining
	default:
		status.Phase = v1.NodeDrainDrained
	}
	return Result{Requeue: status.Phase != v1.NodeDrainDrained}, c.record(ctx, log, node, status)
}

// requestMove asks for a planned move of the Project off node.
func (c *DrainController) requestMove(ctx context.Context, log *zap.Logger, project, node string) error {
	// Any tier: a drain empties the Node, wherever the Project can go.
	err := c.store.RequestEviction(ctx, project, node, DrainReason, "")
	switch {
	case err == nil:
		log.Info("Asked to move project off draining node", zap.String("project", project))
		return nil
	case errors.Is(err, store.ErrConflictState), errors.Is(err, store.ErrNotFound):
		// It moved, stopped or went away since it was listed; the next pass
		// sees where it is now.
		return nil
	default:
		return err
	}
}

// record writes the drain's progress when it changed, keeping Since across
// passes that change nothing, and alerts once when it becomes Incomplete.
func (c *DrainController) record(ctx context.Context, log *zap.Logger, node NodeDrainSnapshot, status *v1.NodeDrainStatus) error {
	prev := node.Status
	status.Since = c.clock.Now().UTC()
	if prev != nil && prev.Phase == status.Phase {
		status.Since = prev.Since
	}
	if prev != nil && prev.Phase == status.Phase && prev.Remaining == status.Remaining && slices.Equal(prev.Blocking, status.Blocking) {
		return nil
	}

	switch status.Phase {
	case v1.NodeDrainDrained:
		log.Info("Node drained: nothing is assigned to it and it owes no cleanup; it can be powered off")
	case v1.NodeDrainIncomplete:
		// TODO(discord): post here too once Discord notifications exist; see
		// the TODO(discord) on raiseRescheduleExhaustedAlert.
		log.Error("Node drain cannot complete without help", zap.Strings("blocking", status.Blocking))
	}
	return ignoreNotFound(c.store.SetNodeDrainStatus(ctx, node.Name, status))
}

// callOff ends a drain that was called off: moves it asked for are cancelled
// where that is still safe, and its record is cleared.
func (c *DrainController) callOff(ctx context.Context, log *zap.Logger, node NodeDrainSnapshot, projects []DrainProjectSnapshot) error {
	for _, p := range projects {
		if p.Eviction == nil || p.Eviction.Reason != DrainReason {
			continue
		}
		cancelled, err := c.store.CancelEviction(ctx, p.Name, node.Name, DrainReason)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if cancelled {
			log.Info("Drain called off; cancelled the project's move", zap.String("project", p.Name))
		} else {
			log.Info("Drain called off; the project's move is past recall and will finish", zap.String("project", p.Name))
		}
	}
	if node.Status == nil {
		return nil
	}
	log.Info("Drain called off")
	return ignoreNotFound(c.store.SetNodeDrainStatus(ctx, node.Name, nil))
}

// owedNotAssigned are the Projects owed cleanup that are no longer assigned to
// the Node, so they are not counted twice.
func owedNotAssigned(owed []string, projects []DrainProjectSnapshot) []string {
	var out []string
	for _, name := range owed {
		if !slices.ContainsFunc(projects, func(p DrainProjectSnapshot) bool { return p.Name == name }) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Seed implements controller.Seeder. A Node update (a drain set or called
// off) and a Project update (a move progressing, a cleanup reported) can both
// advance a drain; the resync covers the retries.
func (c *DrainController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var nodeUpdated, projectUpdated event.Handler
	if c.bus != nil {
		nodeUpdated = c.bus.Subscribe(event.TopicNodeUpdated)
		projectUpdated = c.bus.Subscribe(event.TopicProjectUpdated)
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	resync := func() {
		names, err := c.store.ListNodeDrains(ctx)
		if err != nil {
			log.Warn("Failed to list draining nodes", zap.Error(err))
			return
		}
		for _, n := range names {
			enqueue(n)
		}
	}
	resync()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-nodeUpdated:
			if !ok {
				nodeUpdated = nil
				continue
			}
			enqueue(e.Name)
		case _, ok := <-projectUpdated:
			if !ok {
				projectUpdated = nil
				continue
			}
			resync()
		case <-tick.C:
			resync()
		}
	}
}
