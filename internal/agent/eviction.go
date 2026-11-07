package agent

import (
	"context"
	"errors"
	"sync"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"

	"go.uber.org/zap"
)

// Reasons the agent reports on the EvictionBackup condition.
const (
	evictionBackupSucceeded     = "BackupSucceeded"
	evictionBackupFailed        = "BackupFailed"
	evictionBackupNoObjectStore = "NoObjectStore"
	evictionBackupNotConfirmed  = "MoveNotConfirmed"
	evictionStopFailed          = "StopFailed"
	evictionStoppedNoData       = "StoppedNoData"
	evictionUnsupportedStrategy = "UnsupportedBackupStrategy"
)

// evictionRunner takes the fresh backup a planned move asks for. It is
// satisfied by *backup.Runner.
type evictionRunner interface {
	RunForEviction(ctx context.Context, project *v1.Project, stillWanted func(context.Context) bool) (backupID string, leftStopped bool, err error)
}

// evictionClient is the part of the server API the eviction backups use.
type evictionClient interface {
	GetProject(ctx context.Context, name string) (*v1.Project, error)
	PatchProjectCondition(ctx context.Context, projectName string, fence AssignmentFence, condType v1.ConditionType, status v1.ConditionStatus, reason, message string) error
	ReportWorkloadStopped(ctx context.Context, projectName string, fence AssignmentFence) error
}

// evictionClaimer is the agent-local coordinator, so a Project being stopped
// for a move is not also backed up, restored or cleaned up at the same time.
// It is satisfied by *backup.Coordinator.
type evictionClaimer interface {
	TryClaim(key backup.ResourceKey, op backup.Operation) (release func(), ok bool)
}

// evictionBackups carries out this node's part of a planned move (CARA-119):
// when the server asks for it, stop the Project — taking a fresh backup first
// if it has Managed data — leave it stopped, confirm the workload stopped, and
// report. The server then releases the assignment through its release gate,
// this node cleans up, and the scheduler places the Project elsewhere; the
// node it lands on restores from this backup.
//
// Each backup runs on its own goroutine. Like a periodic backup it can take
// minutes, and the poll loop it would otherwise block also sends heartbeats.
type evictionBackups struct {
	ctx        context.Context
	runner     evictionRunner // nil when this agent has no object store
	containers backup.ContainerController
	claimer    evictionClaimer // nil when this agent has no object store
	client     evictionClient
	logger     *zap.Logger

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// newEvictionBackups creates an evictionBackups whose work runs under ctx.
// runner and claimer may be nil, as on an agent with no object store: a
// Project with Managed data then cannot be moved, since there is nowhere to
// back it up, and one without can.
func newEvictionBackups(ctx context.Context, runner evictionRunner, containers backup.ContainerController, claimer evictionClaimer, client evictionClient, logger *zap.Logger) *evictionBackups {
	return &evictionBackups{
		ctx:        ctx,
		runner:     runner,
		containers: containers,
		claimer:    claimer,
		client:     client,
		logger:     logger,
		running:    make(map[string]bool),
	}
}

// underEviction reports whether p is waiting on this node for a planned
// move's backup. The poll loop leaves such a Project alone: its containers
// are stopped on purpose, and a health check would take that for a failure
// and start them again.
func underEviction(p *v1.Project) bool {
	e := p.Status.Eviction
	return e != nil && e.State == v1.EvictionStateBackingUp && p.Status.Phase == v1.ProjectPhaseRunning
}

// reported reports whether a result for p's current move is already on the
// server, so the backup is not taken twice. A result stamped before the move
// entered BackingUp belongs to an earlier move.
func reported(p *v1.Project) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == v1.ConditionTypeEvictionBackup && !c.LastTransitionTime.Before(p.Status.Eviction.StateSince) {
			return true
		}
	}
	return false
}

// handle starts the backup for p's planned move if it is wanted and not
// already running or reported. It never blocks.
func (b *evictionBackups) handle(p *v1.Project) {
	if !underEviction(p) || reported(p) {
		return
	}

	b.mu.Lock()
	if b.running[p.Name] {
		b.mu.Unlock()
		return
	}
	b.running[p.Name] = true
	b.mu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			b.mu.Lock()
			delete(b.running, p.Name)
			b.mu.Unlock()
		}()
		b.run(p)
	}()
}

// run stops the Project for the move, backing it up first when it has
// Managed data, and reports the result.
func (b *evictionBackups) run(p *v1.Project) {
	log := b.logger.With(zap.String("project", p.Name), zap.String("reason", p.Status.Eviction.Reason))
	fence := fenceForProject(p)

	switch {
	case p.Spec.HasPostgresDifferential():
		// The fresh backup a move takes is an offline one; a
		// PostgresDifferential database is backed up through pgBackRest, which
		// a move cannot do yet. The server refuses such moves before they
		// start; this only guards against a request that got through anyway.
		log.Warn("A planned move cannot back up a PostgresDifferential volume; leaving the project running")
		b.report(log, p, fence, v1.ConditionFalse, evictionUnsupportedStrategy,
			"A planned move cannot take a fresh backup of a PostgresDifferential volume")
		return

	case !p.Spec.HasManagedVolume():
		b.stopOnly(log, p, fence)
		return

	case b.runner == nil:
		log.Error("A planned move needs a backup but this agent has no object store configured")
		b.report(log, p, fence, v1.ConditionFalse, evictionBackupNoObjectStore,
			"This agent has no object store configured, so the project cannot be backed up for the move")
		return
	}

	log.Info("Taking a fresh backup for a planned move")
	backupID, leftStopped, err := b.runner.RunForEviction(b.ctx, p, b.stillWanted(p))
	switch {
	case errors.Is(err, backup.ErrProjectBusy):
		// Another operation holds the Project; the next poll tries again.
		log.Info("Project busy, the move's backup will be retried")
	case err != nil:
		log.Error("The fresh backup for a planned move failed; the project was started again here", zap.Error(err))
		b.report(log, p, fence, v1.ConditionFalse, evictionBackupFailed, err.Error())
	case !leftStopped:
		// The move was called off, or could not be confirmed, while the
		// backup ran. The containers are running again, so the server must
		// not move the Project on the strength of this backup.
		log.Warn("Backup done but the move could not be confirmed; the project was started again here")
		b.report(log, p, fence, v1.ConditionFalse, evictionBackupNotConfirmed,
			"The backup completed but the move could not be confirmed, so the project was started again here")
	default:
		log.Info("Fresh backup committed; leaving the project stopped for the move", zap.String("backup", backupID))
		b.confirmStopped(log, p, fence, evictionBackupSucceeded, backupID)
	}
}

// stopOnly stops a Project that has no Managed data to carry. Nothing needs
// backing up, but the release gate still needs the workload stopped, so the
// old copy is not running beside the new one.
func (b *evictionBackups) stopOnly(log *zap.Logger, p *v1.Project, fence AssignmentFence) {
	if b.claimer != nil {
		release, ok := b.claimer.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpBackup)
		if !ok {
			log.Info("Project busy, stopping it for the move will be retried")
			return
		}
		defer release()
	}

	log.Info("Stopping a project with no Managed data for a planned move")
	if err := b.containers.StopProject(b.ctx, p); err != nil {
		log.Error("Could not stop the project for the move; starting it again", zap.Error(err))
		b.restart(log, p)
		b.report(log, p, fence, v1.ConditionFalse, evictionStopFailed, err.Error())
		return
	}
	if !b.stillWanted(p)(context.WithoutCancel(b.ctx)) {
		log.Warn("Stopped, but the move could not be confirmed; starting the project again")
		b.restart(log, p)
		b.report(log, p, fence, v1.ConditionFalse, evictionBackupNotConfirmed,
			"The project was stopped but the move could not be confirmed, so it was started again here")
		return
	}
	b.confirmStopped(log, p, fence, evictionStoppedNoData, "")
}

// confirmStopped records that the workload is stopped, which is what lets
// the server release the assignment, and then reports the move's result. If
// the evidence is not accepted, nothing is reported: the move keeps waiting
// and the next poll, finding no result, stops (and backs up) again — the
// containers are already stopped, so that costs no further downtime.
func (b *evictionBackups) confirmStopped(log *zap.Logger, p *v1.Project, fence AssignmentFence, reason, message string) {
	if err := b.client.ReportWorkloadStopped(context.WithoutCancel(b.ctx), p.Name, fence); err != nil {
		log.Warn("Failed to confirm the workload stopped; the move will retry", zap.Error(err))
		return
	}
	b.report(log, p, fence, v1.ConditionTrue, reason, message)
}

func (b *evictionBackups) restart(log *zap.Logger, p *v1.Project) {
	if err := b.containers.StartProject(context.WithoutCancel(b.ctx), p); err != nil {
		log.Error("Failed to start the project again after an abandoned move", zap.Error(err))
	}
}

// stillWanted returns the check made once the backup is committed: the move
// must still be in BackingUp, be the same move, and the Project still be
// assigned here. Anything else, including failing to ask, restarts the
// containers — a running Project is the safe side to err on.
func (b *evictionBackups) stillWanted(p *v1.Project) func(context.Context) bool {
	want := *p.Status.Eviction
	return func(ctx context.Context) bool {
		cur, err := b.client.GetProject(ctx, p.Name)
		if err != nil || cur == nil || cur.Status.Eviction == nil {
			return false
		}
		e := cur.Status.Eviction
		return e.State == v1.EvictionStateBackingUp && e.StateSince.Equal(want.StateSince) &&
			cur.Status.Phase == v1.ProjectPhaseRunning && cur.Status.NodeRef == p.Status.NodeRef &&
			cur.ObjectMeta.UID == p.ObjectMeta.UID
	}
}

func (b *evictionBackups) report(log *zap.Logger, p *v1.Project, fence AssignmentFence, status v1.ConditionStatus, reason, message string) {
	if err := b.client.PatchProjectCondition(context.WithoutCancel(b.ctx), p.Name, fence,
		v1.ConditionTypeEvictionBackup, status, reason, message); err != nil {
		// Unreported, the move stays waiting and the next poll takes the
		// backup again; with the containers already stopped that costs no
		// further downtime.
		log.Warn("Failed to report the move's backup result", zap.Error(err))
	}
}

// wait blocks until every backup started so far has finished. For tests.
func (b *evictionBackups) wait() { b.wg.Wait() }
