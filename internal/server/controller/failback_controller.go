package controller

import (
	"context"
	"errors"
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
//     room, and the scheduler may place it nowhere else.
//
// It does not move anything itself: it asks the eviction controller for a
// planned move (fresh backup, then Pending), and the scheduler places the
// Project. A move that started inside the window is left to finish after it.
//
// The anti-flap defences (a stability window for a recovered primary Node,
// backoff for a flapping one, a per-Project cooldown, capacity hysteresis)
// are CARA-122. Until they land, leave the window unset, which keeps this
// controller from moving anything.
type FailbackController struct {
	logger       *zap.Logger
	projects     FailbackStore
	nodes        SchedulerNodeStore
	ledger       *CapacityLedger
	window       *dailywindow.Window
	clock        Clock
	seedInterval time.Duration
}

// NewFailbackController creates a FailbackController that moves Projects only
// inside window. A nil window disables fail-back.
func NewFailbackController(
	logger *zap.Logger,
	projects FailbackStore,
	nodes SchedulerNodeStore,
	window *dailywindow.Window,
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

	candidates, err := c.misplaced(ctx, nodes)
	if err != nil {
		return Result{}, err
	}

	for _, p := range candidates {
		fits, _, err := filterNodes(ctx, c.ledger, nodes, ProjectPlacement{
			Size:          p.Size,
			ExcludedNodes: p.FailedNodes,
			RequiredTier:  v1.NodeTierPrimary,
		})
		if err != nil {
			return Result{}, err
		}
		if len(fits) == 0 {
			continue
		}

		log.Info("Moving project back to the primary tier",
			zap.String("project", p.Name), zap.String("fromNode", p.NodeRef), zap.String("size", string(p.Size)))
		err = c.projects.RequestEviction(ctx, p.Name, p.NodeRef, FailBackReason, v1.NodeTierPrimary)
		if errors.Is(err, store.ErrConflictState) || errors.Is(err, store.ErrNotFound) {
			// It moved or stopped since it was listed; look again next pass.
			log.Debug("Project changed before the fail-back request", zap.String("project", p.Name), zap.Error(err))
			return Result{}, nil
		}
		return Result{}, err
	}
	return Result{}, nil
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
