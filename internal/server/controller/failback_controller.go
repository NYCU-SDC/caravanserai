package controller

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/dailywindow"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

const (
	// FailBackReason is the Reason on a planned move fail-back asks for.
	FailBackReason = "FailBack"

	// failbackInterval is how often fail-back looks for misplaced Projects.
	// Coarse on purpose: a move takes minutes, and the window is hours.
	failbackInterval = time.Minute

	// failbackQueueKey is the only key this controller reconciles; each pass
	// considers the whole cluster.
	failbackQueueKey = "failback"
)

// FailbackPolicy is how cautious fail-back is (CARA-122,
// docs/scheduler-strategy.md §3.3). The primary tier is the less stable one,
// and every move back is downtime, so it is better to move back late than to
// bounce.
type FailbackPolicy struct {
	// StableWindow is how long a primary Node must have been Ready, without
	// a break, before it may receive a Project back.
	StableWindow time.Duration

	// FlapWindow is how far back NotReady transitions are counted. Each one
	// in it beyond the first multiplies StableWindow by BackoffMultiplier,
	// up to MaxStableWindow.
	FlapWindow        time.Duration
	BackoffMultiplier int
	MaxStableWindow   time.Duration

	// Cooldown is how long a Project must have been running on its current
	// Node — however it got there — before fail-back moves it.
	Cooldown time.Duration

	// MinFreePercent is how much of a primary Node's Allocatable, of CPU and
	// of memory each, must still be free once the Project is placed. A Node
	// the Project only just fits on is not used, so fail-back does not keep
	// firing at the edge of capacity.
	MinFreePercent float64
}

// DefaultFailbackPolicy returns the interim values pending product sign-off.
func DefaultFailbackPolicy() FailbackPolicy {
	return FailbackPolicy{
		StableWindow:      30 * time.Minute,
		FlapWindow:        24 * time.Hour,
		BackoffMultiplier: 2,
		MaxStableWindow:   8 * time.Hour,
		Cooldown:          6 * time.Hour,
		MinFreePercent:    20,
	}
}

// stableWindowFor returns how long a Node that went NotReady flaps times in
// the flap window must have been Ready: StableWindow for one flap or none,
// multiplied by BackoffMultiplier for each further one, capped at
// MaxStableWindow.
func (p FailbackPolicy) stableWindowFor(flaps int) time.Duration {
	d := p.StableWindow
	for i := 1; i < flaps; i++ {
		d *= time.Duration(p.BackoffMultiplier)
		if d >= p.MaxStableWindow {
			return p.MaxStableWindow
		}
	}
	return d
}

// FailbackStore is the store surface needed by FailbackController.
type FailbackStore interface {
	// CapacityProjectStore lists the Projects on a Node, for finding the
	// ones on backup Nodes and for the capacity check.
	CapacityProjectStore

	// ListEvictions returns every Project with a planned move in progress.
	ListEvictions(ctx context.Context) ([]*EvictionSnapshot, error)

	// EvictionRequester asks for the move.
	EvictionRequester
}

// FailbackController returns Projects from the backup tier to the primary
// tier once primary has room again (docs/scheduler-strategy.md §3.3, CARA-121).
// Failover moves Projects to backup when primary fails; nothing else ever
// moves them back, because they keep running fine where they are.
//
// Detection is stateless: a Running Project on a backup Node while a primary
// Node could take it is misplaced, by definition. Where it ran before is not
// recorded or needed, so a Project that landed on backup because primary was
// full is brought back too.
//
// A move back is itself downtime, and the Project is not in trouble, so it is
// conservative:
//   - it happens only inside the configured daily window, never as soon as
//     primary recovers;
//   - one fail-back move at a time, the next asked for only once the last is
//     over, each after checking afresh what primary can take;
//   - the move must land on primary — it is not started if primary has no
//     room, and the scheduler may place it nowhere else;
//   - and the FailbackPolicy defences against a flapping primary tier: only
//     a primary Node that has been Ready long enough (longer for one that
//     keeps failing) and that keeps comfortable room after the move may take
//     the Project, and a Project that has only just started where it is is
//     left there.
//
// It does not move anything itself: it asks the eviction controller for a
// planned move (fresh backup, then Pending), and the scheduler places the
// Project, avoiding the primary Nodes fail-back did not accept. A move that
// started inside the window is left to finish after it.
type FailbackController struct {
	logger       *zap.Logger
	projects     FailbackStore
	nodes        SchedulerNodeStore
	ledger       *CapacityLedger
	window       *dailywindow.Window
	policy       FailbackPolicy
	clock        Clock
	seedInterval time.Duration
}

// NewFailbackController creates a FailbackController that moves Projects only
// inside window, as cautiously as policy says. A nil window disables
// fail-back.
func NewFailbackController(
	logger *zap.Logger,
	projects FailbackStore,
	nodes SchedulerNodeStore,
	window *dailywindow.Window,
	policy FailbackPolicy,
	opts ...Option,
) *FailbackController {
	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = failbackInterval
	}
	return &FailbackController{
		logger:       logger,
		projects:     projects,
		nodes:        nodes,
		ledger:       NewCapacityLedger(logger, projects),
		window:       window,
		policy:       policy,
		clock:        o.clock,
		seedInterval: interval,
	}
}

// Name implements Controller.
func (c *FailbackController) Name() string { return "failback" }

// Reconcile implements Controller. The key is always failbackQueueKey.
func (c *FailbackController) Reconcile(ctx context.Context, _ string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()))

	if c.window == nil || !c.window.Contains(c.clock.Now()) {
		return Result{}, nil
	}

	evictions, err := c.projects.ListEvictions(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, e := range evictions {
		if e.Eviction.Reason == FailBackReason {
			log.Debug("A fail-back move is in progress; the next waits for it",
				zap.String("project", e.Name), zap.String("state", string(e.Eviction.State)))
			return Result{}, nil
		}
	}

	nodes, err := c.nodes.ListReadyNodes(ctx)
	if err != nil {
		return Result{}, err
	}

	stable, unstable := c.stablePrimaries(log, nodes)
	if len(stable) == 0 {
		log.Debug("No primary node has been stable long enough to take a project back")
		return Result{}, nil
	}

	candidates, err := c.misplaced(ctx, nodes)
	if err != nil {
		return Result{}, err
	}

	for _, p := range candidates {
		if !c.cooledDown(log, p) {
			continue
		}

		fits, _, err := filterNodes(ctx, c.ledger, stable, ProjectPlacement{
			Size:          p.Size,
			ExcludedNodes: p.FailedNodes,
			RequiredTier:  v1.NodeTierPrimary,
		})
		if err != nil {
			return Result{}, err
		}

		// Of the stable primary Nodes, only those the Project fits on with
		// room to spare are accepted. The scheduler, which prefers the
		// emptiest Node, is told to avoid every other primary Node.
		var accepted []string
		for _, n := range fits {
			if c.comfortable(n, p.Size) {
				accepted = append(accepted, n.Name)
			}
		}
		if len(accepted) == 0 {
			log.Debug("No stable primary node keeps enough room for the project",
				zap.String("project", p.Name), zap.Int("fits", len(fits)))
			continue
		}
		avoid := slices.Clone(unstable)
		for _, n := range stable {
			if !slices.Contains(accepted, n.Name) {
				avoid = append(avoid, n.Name)
			}
		}

		log.Info("Moving project back to the primary tier",
			zap.String("project", p.Name), zap.String("fromNode", p.NodeRef), zap.String("size", string(p.Size)),
			zap.Strings("acceptedNodes", accepted), zap.Strings("avoidNodes", avoid))
		err = c.projects.RequestEviction(ctx, p.Name, p.NodeRef, FailBackReason, v1.NodeTierPrimary, avoid)
		if errors.Is(err, store.ErrConflictState) || errors.Is(err, store.ErrNotFound) {
			// It moved or stopped since it was listed; look again next pass.
			log.Debug("Project changed before the fail-back request", zap.String("project", p.Name), zap.Error(err))
			return Result{}, nil
		}
		return Result{}, err
	}
	return Result{}, nil
}

// stablePrimaries splits the Ready primary Nodes into those stable long
// enough to take a Project back and the names of the rest. A Node whose
// Ready time is unknown is not trusted.
func (c *FailbackController) stablePrimaries(log *zap.Logger, nodes []ReadyNode) (stable []ReadyNode, unstable []string) {
	now := c.clock.Now()
	for _, n := range nodes {
		if v1.NodeTierFromLabels(n.Labels) != v1.NodeTierPrimary {
			continue
		}
		status := v1.NodeStatus{State: v1.NodeStateReady, Conditions: n.Conditions}
		since, known := status.ReadySince()
		flaps := 0
		for _, t := range n.RecentNotReady {
			if now.Sub(t) <= c.policy.FlapWindow {
				flaps++
			}
		}
		need := c.policy.stableWindowFor(flaps)
		if !known || now.Sub(since) < need {
			log.Debug("Primary node not stable long enough for fail-back",
				zap.String("node", n.Name), zap.Bool("readySinceKnown", known),
				zap.Duration("readyFor", now.Sub(since)), zap.Duration("needed", need), zap.Int("flaps", flaps))
			unstable = append(unstable, n.Name)
			continue
		}
		stable = append(stable, n)
	}
	return stable, unstable
}

// cooledDown reports whether the Project has been running where it is for
// at least the cooldown. A Project whose start is unknown is left alone.
func (c *FailbackController) cooledDown(log *zap.Logger, p *ProjectSnapshot) bool {
	phase, ok := phaseCondition(p)
	if !ok || phase.Reason != phaseReasonRunning {
		log.Debug("Project's running time is unknown, not moving it", zap.String("project", p.Name))
		return false
	}
	if runningFor := c.clock.Since(phase.LastTransitionTime); runningFor < c.policy.Cooldown {
		log.Debug("Project moved too recently for fail-back", zap.String("project", p.Name),
			zap.Duration("runningFor", runningFor), zap.Duration("cooldown", c.policy.Cooldown))
		return false
	}
	return true
}

// comfortable reports whether n keeps at least MinFreePercent of its CPU and
// of its memory free once a Project of size is placed on it.
func (c *FailbackController) comfortable(n candidate, size v1.ProjectSize) bool {
	req := size.Requests()
	cpu := freePercent(n.Allocatable.CPUMilli, n.Used.CPUMilli+req.CPUMilli)
	mem := freePercent(n.Allocatable.MemoryBytes, n.Used.MemoryBytes+req.MemoryBytes)
	return cpu >= c.policy.MinFreePercent && mem >= c.policy.MinFreePercent
}

// misplaced returns the Running Projects on backup-tier Nodes that are not
// already being moved, in name order so the choice is the same every pass.
func (c *FailbackController) misplaced(ctx context.Context, nodes []ReadyNode) ([]*ProjectSnapshot, error) {
	var out []*ProjectSnapshot
	for _, n := range nodes {
		if v1.NodeTierFromLabels(n.Labels) != v1.NodeTierBackup {
			continue
		}
		projects, err := c.projects.ListProjectsByNodeRef(ctx, n.Name, []v1.ProjectPhase{v1.ProjectPhaseRunning})
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			if !p.Evicting {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Seed implements controller.Seeder: fail-back looks again on a fixed tick.
func (c *FailbackController) Seed(ctx context.Context, enqueue func(name string)) {
	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	enqueue(failbackQueueKey)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			enqueue(failbackQueueKey)
		}
	}
}
