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

	decision, err := decideRestore(dataRoot, p)
	if err != nil {
		return err
	}

	switch decision {
	case restore.DecisionSkip:
		log.Debug("Local volume data is authoritative, skipping restore")
		return nil

	case restore.DecisionAdoptExisting:
		// Data with no marker: a Project born on this node, or one predating
		// markers. Record ownership so future passes skip cleanly, and never
		// overwrite what is already there.
		log.Info("Adopting existing volume data without restoring")
		return restore.WriteMarker(dataRoot, p.Namespace, p.Name, "", nowUTC())

	case restore.DecisionRestore:
		return runRestore(ctx, restorer, p, log)

	default:
		return fmt.Errorf("agent: unhandled restore decision %v", decision)
	}
}

// runRestore resolves the newest generation and puts it on disk, treating
// "never backed up" as a normal starting state and every other failure as
// grounds to keep the Project down.
func runRestore(ctx context.Context, restorer *restore.Restorer, p *v1.Project, log *zap.Logger) error {
	backupID, err := restorer.ResolveLatest(ctx, p.Namespace, p.Name)
	switch {
	case err == nil:
		log.Info("Restoring volumes from generation", zap.String("backupID", backupID))
		return restorer.RestoreGeneration(ctx, p, backupID)

	case errors.Is(err, restore.ErrNeverBackedUp):
		// Nothing has ever been backed up under this name, so there is no
		// generation to be missing. Starting with empty volumes is correct.
		//
		// Note this is emphatically not the same as a generation that has gone
		// missing (restore.ErrGenerationMissing), which falls through to the
		// default branch and keeps the Project down — backups demonstrably
		// exist there, and starting empty would present real data loss as a
		// successful start.
		log.Info("No backup generation exists; initialising empty volumes")
		return restorer.InitializeEmpty(p)

	default:
		return err
	}
}

// decideRestore gathers the three facts the decision rests on and applies the
// rule. Kept separate so the fact-gathering is not tangled with the policy.
func decideRestore(dataRoot string, p *v1.Project) (restore.Decision, error) {
	stagingPresent, err := restore.StagingPresent(dataRoot, p.Namespace, p.Name)
	if err != nil {
		return 0, err
	}

	marker, err := restore.ReadMarker(dataRoot, p.Namespace, p.Name)
	if err != nil {
		return 0, err
	}

	volumesHaveData, err := restore.VolumesHaveData(dataRoot, p.Namespace, p.Name, p.Spec.Volumes)
	if err != nil {
		return 0, err
	}

	return restore.Decide(stagingPresent, marker != nil, volumesHaveData), nil
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
