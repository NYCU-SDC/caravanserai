package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

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
	ReportWorkloadStoppedForMove(ctx context.Context, projectName string, fence AssignmentFence, moveStateSince time.Time) error
}

// evictionRuntime is the part of the container runtime a move uses: stopping
// and, when the move is called off, starting the Project again, and the
// label-based stop and listing that make sure no container of the assignment
// still runs before its stop is reported.
type evictionRuntime interface {
	backup.ContainerController
	StopAssignment(ctx context.Context, project *v1.Project) error
	ProjectContainers(ctx context.Context, project *v1.Project) ([]docker.ProjectContainer, error)
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
	containers evictionRuntime
	claimer    evictionClaimer // nil when this agent has no object store
	client     evictionClient
	latches    *StopLatches
	logger     *zap.Logger

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// newEvictionBackups creates an evictionBackups whose work runs under ctx.
// runner and claimer may be nil, as on an agent with no object store: a
// Project with Managed data then cannot be moved, since there is nowhere to
// back it up, and one without can.
func newEvictionBackups(ctx context.Context, runner evictionRunner, containers evictionRuntime, claimer evictionClaimer, client evictionClient, latches *StopLatches, logger *zap.Logger) *evictionBackups {
	return &evictionBackups{
		ctx:        ctx,
		runner:     runner,
		containers: containers,
		claimer:    claimer,
		client:     client,
		latches:    latches,
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
	// The server holds this move's stop evidence: it moves the Project on
	// that alone, whatever result reaches it. Stopping or backing up again
	// could only end in a restart, and the Project may already be starting
	// on another node.
	if workloadLatched(p) {
		return
	}
	// Recorded here but not, as far as the server said, accepted: the
	// evidence is sent again, and the containers are not touched.
	resend, _ := b.latches.Holds(p)

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
		if resend {
			b.resendStopped(p)
			return
		}
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
		// The backup stopped the services the spec names. A container of
		// this assignment for a service the spec has dropped is stopped
		// here, so it is not left running beside the moved copy.
		if err := b.containers.StopAssignment(context.WithoutCancel(b.ctx), p); err != nil {
			b.abandonStop(log, p, fence, "Could not stop every container of the project for the move: "+err.Error())
			return
		}
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
	// By label, not by spec: a stopped leftover of another assignment under
	// a service's name does not refuse the stop, and a service the spec has
	// dropped is stopped too.
	if err := b.containers.StopAssignment(b.ctx, p); err != nil {
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
// the server release the assignment, and then reports the move's result.
//
// The stop is confirmed by what Docker lists for the Project, not by the
// services the spec names: nothing may run, of this assignment or another,
// when the evidence is sent. The caller has stopped the containers with
// StopAssignment, which also stops a service the spec has since dropped. Before it is sent the
// stop is recorded on this node, so nothing here starts the assignment again
// from then on.
//
// If the evidence is refused the move was called off, or the assignment is no
// longer this node's: the record is dropped and the Project left to the poll
// loop, which starts it again if it is still assigned here. If the answer is
// unknown the record stays, and the next poll sends the evidence again
// without touching the containers.
func (b *evictionBackups) confirmStopped(log *zap.Logger, p *v1.Project, fence AssignmentFence, reason, message string) {
	listed, err := b.containers.ProjectContainers(context.WithoutCancel(b.ctx), p)
	if err != nil {
		b.abandonStop(log, p, fence, "Could not confirm the project's containers stopped for the move: "+err.Error())
		return
	}
	if running := append(runningOf(listed, true), runningOf(listed, false)...); len(running) > 0 {
		b.abandonStop(log, p, fence, "Containers still running, so the move cannot go on: "+strings.Join(running, ", "))
		return
	}
	if err := b.latches.Record(p, time.Now()); err != nil {
		b.abandonStop(log, p, fence, "Could not record the stop on this node: "+err.Error())
		return
	}
	if !b.sendStopped(log, p, fence) {
		return
	}
	b.report(log, p, fence, v1.ConditionTrue, reason, message)
}

// resendStopped sends the stop evidence of a move whose stop this node
// recorded but the server has not been seen to accept. The containers are
// left as they are: a recorded stop is never undone here.
func (b *evictionBackups) resendStopped(p *v1.Project) {
	log := b.logger.With(zap.String("project", p.Name), zap.String("reason", p.Status.Eviction.Reason))
	fence := fenceForProject(p)
	if b.sendStopped(log, p, fence) {
		log.Info("Stop evidence for the planned move sent again")
	}
}

// sendStopped sends the evidence and reports whether the server took it.
func (b *evictionBackups) sendStopped(log *zap.Logger, p *v1.Project, fence AssignmentFence) bool {
	// Tied to this move: if it was called off meanwhile, the server refuses
	// the evidence instead of keeping it on a Project that runs here again.
	err := b.client.ReportWorkloadStoppedForMove(context.WithoutCancel(b.ctx), p.Name, fence, p.Status.Eviction.StateSince)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrStaleAssignment) || errors.Is(err, ErrProjectNotFound):
		if fErr := b.latches.Forget(p.Namespace, p.Name); fErr != nil {
			log.Warn("Could not drop the local stop record", zap.Error(fErr))
		}
		log.Info("The server refused the move's stop evidence; the move was called off or the assignment moved on", zap.Error(err))
	default:
		log.Warn("Failed to confirm the workload stopped; it will be sent again", zap.Error(err))
	}
	return false
}

// abandonStop starts the Project again and reports why the move cannot go
// on. Nothing was reported stopped, so starting it here is still allowed.
func (b *evictionBackups) abandonStop(log *zap.Logger, p *v1.Project, fence AssignmentFence, message string) {
	log.Error("The planned move cannot go on; starting the project again", zap.String("detail", message))
	b.restart(log, p)
	b.report(log, p, fence, v1.ConditionFalse, evictionStopFailed, message)
}

// restart starts the Project again after a move that did not happen. A stop
// recorded on this node is never undone: the guarded runtime would refuse
// the start anyway, and this says why in the log rather than in an error.
func (b *evictionBackups) restart(log *zap.Logger, p *v1.Project) {
	if held, err := b.latches.Holds(p); held {
		log.Warn("Not starting the project: this node has reported it stopped", zap.Error(err))
		return
	}
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
