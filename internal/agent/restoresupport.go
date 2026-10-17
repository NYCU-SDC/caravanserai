package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"go.uber.org/zap"
)

// ensureVolumeData puts a Project's Managed volumes in place before its
// containers are created, and reports whether the caller may proceed.
//
// Returning an error means the Project must not start: its data is either
// unknown or known to be wrong, and running containers against that is worse
// than not running them at all.
//
// objectStore false means this agent has no object store configured. Managed
// volumes still work — they persist locally — they are simply never restored,
// and placement is not decided for them. A PostgresDifferential Project is the
// exception: it is decided from local provenance on every agent, because
// starting its PostgreSQL on an empty directory would initialize a new
// database in place of one it used to have.
// The dangerous case, a Project that asks to be backed up landing on a node
// that cannot back it up, is already failed by the backup path.
func ensureVolumeData(
	ctx context.Context,
	restorer *restore.Restorer,
	coordinator *backup.Coordinator,
	objectStore bool,
	dataRoot string,
	nodeName string,
	p *v1.Project,
	logger *zap.Logger,
) error {
	// Both are checked because reconcileProjects treats a nil Coordinator on
	// the same BackupSupport as a supported state; the two call sites must not
	// disagree about whether the field is optional.
	if restorer == nil || coordinator == nil {
		return nil
	}
	if !hasManagedVolume(p.Spec.Volumes) {
		return nil
	}
	postgresDifferential := p.Spec.HasPostgresDifferential()
	if !objectStore && !postgresDifferential {
		return nil
	}

	key := backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}
	log := logger.With(zap.String("project", key.String()))

	// Restore is exclusive with backup, terminate, drain and GC: all of them
	// move the same bytes.
	release, ok := coordinator.TryClaim(key, backup.OpRestore)
	if !ok {
		op, _ := coordinator.Current(key)
		log.Info("Deferring volume restore, project is busy", zap.String("operation", string(op)))
		// Not an error — the Project simply waits for the next tick rather
		// than being failed for losing a race.
		return errDeferred
	}
	defer release()

	want := restore.ProvenanceFor(p, nodeName, "")
	outcome, err := decideRestore(dataRoot, want, p)
	if err != nil {
		return err
	}

	if outcome.Decision == restore.DecisionBlock {
		return &restore.PlacementBlockedError{Reason: outcome.Reason, Detail: outcome.Detail}
	}

	switch outcome.Decision {
	case restore.DecisionSkip:
		log.Debug("Local provenance matches this assignment, skipping restore")
		return nil

	case restore.DecisionInitializeEmpty:
		log.Info("Project has never been assigned anywhere; initialising empty volumes")
		return restorer.InitializeEmpty(p)

	case restore.DecisionRestore:
		if postgresDifferential {
			// Decided before anything reads latest.json: the offline archives
			// it points to are not this Project's backups, and may not even
			// be this Project's.
			return &restore.PlacementBlockedError{
				Reason: restore.BlockStrategyRestoreUnavailable,
				Detail: "this Project's PostgresDifferential data would have to be restored from the object store, which is not supported yet",
			}
		}
		return runRestore(ctx, restorer, p, log)

	default:
		return fmt.Errorf("agent: unhandled restore decision %v", outcome.Decision)
	}
}

// runRestore puts the generation the server committed on disk. It reads that
// from the Project rather than from the object store, which any Agent that can
// reach the bucket could write to (ADR-0024 C1).
//
// It is reached only when Decide has established that this node has no local
// data it may use, so every way of failing to produce a generation here leaves
// the Project with nothing — and none of them may be answered by creating
// empty directories and calling that a successful start.
func runRestore(ctx context.Context, restorer *restore.Restorer, p *v1.Project, log *zap.Logger) error {
	backupID := p.Status.Recovery.CommittedBackupID()
	if backupID == "" {
		// This inference only holds in one direction: a Project that has never
		// been placed anywhere has nothing to lose, and Decide has already
		// handled that case by returning DecisionInitializeEmpty.
		//
		// Arriving here means the opposite: this Project has been placed
		// before, or a restore was already in flight. Starting empty would
		// present that as a successful deployment, then let the Backup
		// Supervisor commit the empty result as the restore source.
		return &restore.PlacementBlockedError{
			Reason: restore.BlockNoRestoreSource,
			Detail: "this Project has been placed before, but the server has no committed backup to restore from",
		}
	}
	log.Info("Restoring volumes from generation", zap.String("backupID", backupID))
	return restorer.RestoreGeneration(ctx, p, backupID)
}

// decideRestore gathers the facts the decision rests on and applies the rule.
// Kept separate so the fact-gathering is not tangled with the policy.
func decideRestore(dataRoot string, want restore.Provenance, p *v1.Project) (restore.Outcome, error) {
	var state restore.PlacementState

	var err error
	if state.StagingPresent, err = restore.StagingPresent(dataRoot, p.Namespace, p.Name); err != nil {
		return restore.Outcome{}, err
	}

	// A marker that cannot be read is carried into the decision rather than
	// returned as an error: "unreadable" is one of the states the rule has an
	// answer for, and that answer is to block rather than to fail the Project.
	state.Marker, state.MarkerErr = restore.ReadMarker(dataRoot, p.Namespace, p.Name)

	if state.Volumes, err = restore.SurveyVolumes(dataRoot, p.Namespace, p.Name, p.Spec.Volumes); err != nil {
		return restore.Outcome{}, err
	}

	return restore.Decide(state, want, p.Status.AssignmentHistory), nil
}

func hasManagedVolume(volumes []v1.VolumeDef) bool {
	for _, v := range volumes {
		if v.Type == v1.VolumeTypeManaged {
			return true
		}
	}
	return false
}

// errDeferred signals that an operation could not run this tick because
// another held the Project, and that this is not a fault. The caller should
// leave the Project's status alone and try again on the next pass.
var errDeferred = errors.New("agent: operation deferred, project busy")

// nowUTC exists so the adopt path records a timestamp without threading a
// clock through every call site; restore's own paths use the Restorer's clock.
func nowUTC() time.Time { return time.Now().UTC() }
