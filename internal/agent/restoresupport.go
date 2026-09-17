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
// A nil restorer means this agent has no object store configured. Managed
// volumes still work — they persist locally — they are simply never restored.
// The dangerous case, a Project that asks to be backed up landing on a node
// that cannot back it up, is already failed by the backup path.
func ensureVolumeData(
	ctx context.Context,
	restorer *restore.Restorer,
	coordinator *backup.Coordinator,
	dataRoot string,
	nodeName string,
	strict bool,
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
		if strict || !shadowFallbackAllowed(outcome.Reason) {
			return &restore.PlacementBlockedError{Reason: outcome.Reason, Detail: outcome.Detail}
		}
		// Shadow mode reports the judgement and then does what the previous
		// release would have done. It exists so an operator can see how the
		// rule lands on real deployments before it starts refusing to place
		// Projects — and it is at Warn precisely because the defect it
		// replaces spent a week invisible at Debug.
		log.Warn("Provenance check would block this placement (shadow mode)",
			zap.String("reason", string(outcome.Reason)),
			zap.String("detail", outcome.Detail),
			zap.String("projectUID", want.ProjectUID),
			zap.String("nodeName", want.NodeName),
			zap.Int64("assignmentGeneration", want.AssignmentGeneration),
			zap.Int("markerVersion", markerVersionOf(dataRoot, p)))
		outcome = restore.Outcome{Decision: restore.DecisionSkip}
	}

	switch outcome.Decision {
	case restore.DecisionSkip:
		log.Debug("Local provenance matches this assignment, skipping restore")
		return nil

	case restore.DecisionInitializeEmpty:
		log.Info("Project has never been assigned anywhere; initialising empty volumes")
		return restorer.InitializeEmpty(p)

	case restore.DecisionRestore:
		return runRestore(ctx, restorer, p, log)

	default:
		return fmt.Errorf("agent: unhandled restore decision %v", outcome.Decision)
	}
}

// shadowFallbackAllowed reports whether a block may be downgraded to the
// pre-provenance behaviour while strict mode is off.
//
// Shadow mode exists to keep behaviour unchanged, and for most reasons that is
// what it does: a marker naming another assignment, or one from an older
// schema, used to be enough to skip the restore, so reporting and skipping is
// exactly what the previous release did.
//
// A marker that cannot be read is the exception, because there the previous
// release was already strict — ReadMarker returned an error and the placement
// stopped. Downgrading it would make this change *weaken* an existing
// protection under its own default, which is the one thing a compatibility
// mode must never do.
func shadowFallbackAllowed(reason restore.BlockReason) bool {
	return reason != restore.BlockCorruptProvenance
}

// markerVersionOf reports the on-disk marker schema version for the shadow
// log, or zero when there is none to read. Failures are not surfaced: this
// only annotates a line whose decision has already been made.
func markerVersionOf(dataRoot string, p *v1.Project) int {
	m, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	if err != nil || m == nil {
		return 0
	}
	return m.Version
}

// runRestore resolves the newest generation and puts it on disk.
//
// It is reached only when Decide has established that this node has no local
// data it may use, so every way of failing to produce a generation here leaves
// the Project with nothing — and none of them may be answered by creating
// empty directories and calling that a successful start.
func runRestore(ctx context.Context, restorer *restore.Restorer, p *v1.Project, log *zap.Logger) error {
	backupID, err := restorer.ResolveLatest(ctx, p.Namespace, p.Name)
	switch {
	case err == nil:
		log.Info("Restoring volumes from generation", zap.String("backupID", backupID))
		return restorer.RestoreGeneration(ctx, p, backupID)

	case errors.Is(err, restore.ErrNeverBackedUp):
		// "No latest.json" used to be read as "this Project has never held
		// data", and the response was to start empty. That inference only
		// holds for a Project that has never been placed anywhere, and Decide
		// has already handled that case by returning DecisionInitializeEmpty
		// before anything reached the object store.
		//
		// Arriving here means the opposite: this Project has been placed
		// before, or a restore was already in flight. A missing pointer is
		// then a fault — a bucket typo, an object-store outage, a backup that
		// never actually completed — and starting empty would present it as a
		// successful deployment, then let the Backup Supervisor archive the
		// empty result over the generation that held the real data.
		return &restore.PlacementBlockedError{
			Reason: restore.BlockNoRestoreSource,
			Detail: "this Project has been placed before, but the object store holds no complete backup to restore from",
		}

	default:
		return err
	}
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
