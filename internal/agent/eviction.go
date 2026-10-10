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
}

// evictionBackups carries out this node's part of a planned move (CARA-119):
// when the server asks for it, stop the Project, take a fresh backup, leave it
// stopped, and report. The server then moves the Project and the scheduler
// places it elsewhere; the node it lands on restores from this backup.
//
// Each backup runs on its own goroutine. Like a periodic backup it can take
// minutes, and the poll loop it would otherwise block also sends heartbeats.
type evictionBackups struct {
	ctx    context.Context
	runner evictionRunner // nil when this agent has no object store
	client evictionClient
	logger *zap.Logger

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// newEvictionBackups creates an evictionBackups whose backups run under ctx.
// runner may be nil; every move asking for a backup then fails, since there is
// nowhere to put one.
func newEvictionBackups(ctx context.Context, runner evictionRunner, client evictionClient, logger *zap.Logger) *evictionBackups {
	return &evictionBackups{
		ctx:     ctx,
		runner:  runner,
		client:  client,
		logger:  logger,
		running: make(map[string]bool),
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

// run takes the backup and reports the result.
func (b *evictionBackups) run(p *v1.Project) {
	log := b.logger.With(zap.String("project", p.Name), zap.String("reason", p.Status.Eviction.Reason))
	fence := fenceForProject(p)

	if b.runner == nil {
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
		b.report(log, p, fence, v1.ConditionTrue, evictionBackupSucceeded, backupID)
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
