package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// ContainerController stops and starts a Project's containers without
// destroying them. Backup needs the volumes quiesced but the containers
// intact, which neither ReconcileProject nor RemoveProject provides.
type ContainerController interface {
	StopProject(ctx context.Context, project *v1.Project) error
	StartProject(ctx context.Context, project *v1.Project) error
}

// OwnershipResolver reports whether this node should still be running a
// Project. It is consulted after a backup finishes, because the answer may
// have changed while the containers were stopped — see
// ShouldRestartContainers.
type OwnershipResolver interface {
	Resolve(ctx context.Context, key ResourceKey) Ownership
}

// ConditionReporter records the Maintenance condition on the server. It
// exists for observability and as an input to the future recovery
// controller; it is never what protects correctness, so every method's
// failure is logged and ignored rather than aborting a backup.
type ConditionReporter interface {
	SetMaintenance(ctx context.Context, key ResourceKey, reason string) error
	ClearMaintenance(ctx context.Context, key ResourceKey) error
}

// RouteRefresher re-points the ingress proxy at a Project's containers after
// they are restarted, since restarting assigns new bridge IPs.
type RouteRefresher interface {
	Refresh(ctx context.Context, project *v1.Project)
}

// ErrNoManagedVolumes is returned by Run when a Project declares no Managed
// volumes. It is not a failure: there is simply nothing to capture, and the
// containers are never stopped.
var ErrNoManagedVolumes = errors.New("backup: project has no Managed volumes")

// ErrInsufficientDiskSpace is returned when the staging filesystem is below
// the configured minimum. It is raised before containers are stopped, so a
// full disk costs no downtime.
var ErrInsufficientDiskSpace = errors.New("backup: insufficient free disk space")

// DefaultStagingDir returns where archives are staged for a given data root.
// Staging lives under the data root so it shares a filesystem with the volumes
// being archived, which keeps the free-space precondition meaningful.
func DefaultStagingDir(dataRoot string) string {
	return filepath.Join(dataRoot, "staging")
}

// CleanStaging removes archives left behind by a run that never reached its
// deferred cleanup — an agent killed mid-backup, or a host that lost power.
//
// A Runner removes its own staging directory on every exit path, so anything
// still present at startup is by definition orphaned: it belongs to a process
// that no longer exists. Left alone it would occupy space equal to a full set
// of volume archives, indefinitely, and eventually trip the free-space
// precondition that stops future backups from running.
//
// Callers should invoke this before the agent begins reconciling, and treat
// failure as non-fatal: leftover archives waste space but corrupt nothing.
func CleanStaging(stagingDir string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("backup: read staging dir %q: %w", stagingDir, err)
	}

	var firstErr error
	for _, entry := range entries {
		path := filepath.Join(stagingDir, entry.Name())
		if rmErr := os.RemoveAll(path); rmErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("backup: remove stale staging %q: %w", path, rmErr)
		}
	}
	return firstErr
}

// Config holds the settings a Runner needs that come from AgentConfig.
type Config struct {
	// DataRoot is the agent-owned directory Managed volumes live under.
	DataRoot string
	// StagingDir is where archives are written before upload. It defaults to
	// {DataRoot}/staging and should be on the same filesystem as DataRoot.
	StagingDir string
	// NodeName is recorded in each manifest for operator diagnosis.
	NodeName string
	// CaraVersion is recorded in each manifest.
	CaraVersion string
	// MinFreeBytes aborts a run before stopping containers if the staging
	// filesystem has less free space than this. Zero disables the check.
	MinFreeBytes uint64
	// UploadTimeout bounds the archive+upload phase. Zero means no bound
	// beyond the caller's context.
	UploadTimeout time.Duration
}

// Runner performs one Project-level backup generation. It owns the ordering
// guarantees CARA-59 depends on — stop before archive, verify before
// manifest, manifest before latest.json, restart on every exit path — while
// delegating every side effect to a narrow interface so the whole flow is
// testable without Docker or a live object store.
type Runner struct {
	coordinator *Coordinator
	containers  ContainerController
	store       objectstore.ObjectStore
	ownership   OwnershipResolver
	conditions  ConditionReporter
	routes      RouteRefresher
	cfg         Config
	logger      *zap.Logger

	// now and freeBytes are injectable so tests can control timestamps and
	// simulate a full disk.
	now       func() time.Time
	freeBytes func(path string) (uint64, error)
}

// NewRunner creates a Runner. store may be nil, in which case Run refuses to
// proceed for a Project that declares Managed volumes — a Project whose data
// is meant to be backed up must never run silently unbacked.
func NewRunner(
	coordinator *Coordinator,
	containers ContainerController,
	store objectstore.ObjectStore,
	ownership OwnershipResolver,
	conditions ConditionReporter,
	routes RouteRefresher,
	cfg Config,
	logger *zap.Logger,
) *Runner {
	if cfg.StagingDir == "" {
		cfg.StagingDir = DefaultStagingDir(cfg.DataRoot)
	}
	return &Runner{
		coordinator: coordinator,
		containers:  containers,
		store:       store,
		ownership:   ownership,
		conditions:  conditions,
		routes:      routes,
		cfg:         cfg,
		logger:      logger,
		now:         time.Now,
		freeBytes:   freeBytesOn,
	}
}

// Run performs one backup generation for project.
//
// The flow, and why it is ordered this way:
//
//  1. Claim the Project so nothing else touches it and the poll loop skips
//     it — this is what keeps the phase at Running throughout.
//  2. Check preconditions before stopping anything, so a misconfiguration
//     costs no downtime.
//  3. Stop containers, archive every Managed volume, upload and verify each.
//  4. Write manifest.json only once every archive verified, then latest.json
//     only once the manifest is written. A crash at any point leaves
//     latest.json pointing at the previous complete generation.
//  5. On every exit path — success, failure, timeout, cancellation — restart
//     the containers if this node still owns the Project, refresh routes,
//     clear the Maintenance condition, and release the claim.
//
// Returning ErrNoManagedVolumes means the Project was left untouched.
func (r *Runner) Run(ctx context.Context, project *v1.Project) (err error) {
	key := ResourceKey{Namespace: project.Namespace, Name: project.Name}
	log := r.logger.With(zap.String("project", key.String()))

	volumes := managedVolumes(project.Spec)
	if len(volumes) == 0 {
		return ErrNoManagedVolumes
	}

	release, ok := r.coordinator.TryClaim(key, OpBackup)
	if !ok {
		op, _ := r.coordinator.Current(key)
		log.Info("Skipping backup, project is busy", zap.String("operation", string(op)))
		return nil
	}
	// Registered first so it runs last: nothing else may claim the Project
	// until the containers are back and the condition is cleared.
	defer release()

	// Preconditions, all checked while the containers are still running.
	if r.store == nil {
		return fmt.Errorf("backup: project %s declares Managed volumes but this agent has no object store configured", key)
	}
	if err := r.checkDiskSpace(); err != nil {
		return err
	}

	backupID, err := NewBackupID(r.now())
	if err != nil {
		return err
	}
	log = log.With(zap.String("backupID", backupID))

	if err := r.conditions.SetMaintenance(ctx, key, "BackingUp"); err != nil {
		// Correctness is already protected by the claim above; losing the
		// condition only costs visibility.
		log.Warn("Failed to set Maintenance condition", zap.Error(err))
	}
	defer func() {
		if clearErr := r.conditions.ClearMaintenance(context.WithoutCancel(ctx), key); clearErr != nil {
			log.Warn("Failed to clear Maintenance condition", zap.Error(clearErr))
		}
	}()

	log.Info("Starting backup", zap.Int("volumes", len(volumes)))

	// Registered *before* the stop is attempted, not after: StopProject works
	// through the services one at a time, so a failure partway leaves some
	// already stopped. Registering afterwards would return early on that
	// error and strand them. Runs before the condition is cleared and before
	// the claim is released.
	defer func() {
		r.restoreService(ctx, project, key, err, log)
	}()

	if err = r.containers.StopProject(ctx, project); err != nil {
		return fmt.Errorf("backup: stop containers: %w", err)
	}

	stagingDir := filepath.Join(r.cfg.StagingDir, backupID)
	if mkErr := os.MkdirAll(stagingDir, 0o700); mkErr != nil {
		return fmt.Errorf("backup: create staging dir %q: %w", stagingDir, mkErr)
	}
	defer func() {
		if rmErr := os.RemoveAll(stagingDir); rmErr != nil {
			log.Warn("Failed to remove staging directory",
				zap.String("path", stagingDir), zap.Error(rmErr))
		}
	}()

	uploadCtx := ctx
	if r.cfg.UploadTimeout > 0 {
		var cancel context.CancelFunc
		uploadCtx, cancel = context.WithTimeout(ctx, r.cfg.UploadTimeout)
		defer cancel()
	}

	entries, err := r.captureVolumes(uploadCtx, project, key, backupID, volumes, stagingDir, log)
	if err != nil {
		return err
	}

	// Every archive is uploaded and verified; only now may the generation be
	// declared complete.
	if err = r.commitGeneration(uploadCtx, key, backupID, project, entries, log); err != nil {
		return err
	}

	log.Info("Backup complete", zap.Int("volumes", len(entries)))
	return nil
}

// captureVolumes archives, uploads and verifies every Managed volume,
// returning the manifest entries describing them. An error here means the
// generation is abandoned and no manifest or latest.json is written.
func (r *Runner) captureVolumes(
	ctx context.Context,
	project *v1.Project,
	key ResourceKey,
	backupID string,
	volumes []v1.VolumeDef,
	stagingDir string,
	log *zap.Logger,
) ([]VolumeManifestEntry, error) {
	entries := make([]VolumeManifestEntry, 0, len(volumes))

	for _, vol := range volumes {
		hostPath, err := caravolume.HostPath(r.cfg.DataRoot, key.Namespace, key.Name, vol.Name)
		if err != nil {
			return nil, fmt.Errorf("backup: resolve volume %q: %w", vol.Name, err)
		}

		archivePath := filepath.Join(stagingDir, vol.Name+".tar.gz")
		result, err := Archive(hostPath, archivePath)
		if err != nil {
			return nil, fmt.Errorf("backup: archive volume %q: %w", vol.Name, err)
		}

		archiveKey, err := ArchiveKey(key.Namespace, key.Name, backupID, vol.Name)
		if err != nil {
			return nil, err
		}

		if err := r.uploadAndVerify(ctx, archivePath, archiveKey, result); err != nil {
			return nil, fmt.Errorf("backup: volume %q: %w", vol.Name, err)
		}

		log.Info("Volume archived and uploaded",
			zap.String("volume", vol.Name),
			zap.Int64("sizeBytes", result.SizeBytes))

		entries = append(entries, VolumeManifestEntry{
			Name:       vol.Name,
			ArchiveKey: archiveKey,
			SizeBytes:  result.SizeBytes,
			SHA256:     result.SHA256,
		})
	}

	return entries, nil
}

// uploadAndVerify uploads one archive and confirms the store received it
// intact. Verifying before the manifest is written is what stops latest.json
// ever pointing at a truncated archive.
func (r *Runner) uploadAndVerify(ctx context.Context, archivePath, key string, result ArchiveResult) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("backup: open archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := r.store.Put(ctx, key, f, objectstore.PutOptions{
		ContentType: "application/gzip",
		Size:        result.SizeBytes,
	}); err != nil {
		return fmt.Errorf("backup: upload: %w", err)
	}

	meta, err := r.store.Head(ctx, key)
	if err != nil {
		return fmt.Errorf("backup: verify upload: %w", err)
	}
	if meta.Size != result.SizeBytes {
		return fmt.Errorf("backup: verify upload: stored size %d does not match archive size %d",
			meta.Size, result.SizeBytes)
	}
	return nil
}

// commitGeneration writes the manifest and then latest.json, in that order.
// latest.json is the only mutable object in the layout, and it is written
// last so an interrupted run always leaves the previous generation current.
func (r *Runner) commitGeneration(
	ctx context.Context,
	key ResourceKey,
	backupID string,
	project *v1.Project,
	entries []VolumeManifestEntry,
	log *zap.Logger,
) error {
	manifestKey, err := ManifestKey(key.Namespace, key.Name, backupID)
	if err != nil {
		return err
	}

	manifest := BuildManifest(
		backupID, key.Namespace, key.Name,
		r.cfg.NodeName, r.cfg.CaraVersion,
		project.ResourceVersion, r.now(), entries,
	)
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		return err
	}
	if err := r.putJSON(ctx, manifestKey, manifestBytes); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}

	latestKey, err := LatestKey(key.Namespace, key.Name)
	if err != nil {
		return err
	}
	latestBytes, err := NewLatest(backupID, manifestKey, r.now()).Marshal()
	if err != nil {
		return err
	}
	if err := r.putJSON(ctx, latestKey, latestBytes); err != nil {
		// The generation is intact in the store but unreferenced. Failing
		// loudly is right: a silent success here would let the operator
		// believe a restore point exists that CARA-60 cannot find.
		return fmt.Errorf("backup: update latest pointer: %w", err)
	}

	log.Debug("Generation committed", zap.String("manifestKey", manifestKey))
	return nil
}

func (r *Runner) putJSON(ctx context.Context, key string, body []byte) error {
	_, err := r.store.Put(ctx, key, bytes.NewReader(body), objectstore.PutOptions{
		ContentType: "application/json",
		Size:        int64(len(body)),
	})
	return err
}

// restoreService brings the Project back after a backup, on every exit path.
// Whether the containers are restarted depends on why the run ended and on
// whether this node still owns the Project — see ShouldRestartContainers.
func (r *Runner) restoreService(ctx context.Context, project *v1.Project, key ResourceKey, runErr error, log *zap.Logger) {
	reason := classifyExit(ctx, runErr)

	// The caller's context may already be cancelled; ownership and restart
	// still need to run, so detach from cancellation while keeping values.
	cleanupCtx := context.WithoutCancel(ctx)

	ownership := r.ownership.Resolve(cleanupCtx, key)
	if !ShouldRestartContainers(reason, ownership) {
		log.Info("Leaving containers stopped after backup",
			zap.String("reason", reason.String()),
			zap.String("ownership", ownership.String()))
		return
	}

	if err := r.containers.StartProject(cleanupCtx, project); err != nil {
		log.Error("Failed to restart containers after backup",
			zap.String("reason", reason.String()), zap.Error(err))
		return
	}
	log.Info("Containers restarted after backup", zap.String("reason", reason.String()))

	if r.routes != nil {
		// Restarted containers get new bridge IPs; without this the proxy
		// points at the old ones until the next poll.
		r.routes.Refresh(cleanupCtx, project)
	}
}

// checkDiskSpace refuses to start a backup that would run the staging
// filesystem below the configured floor. Checked before containers stop so a
// full disk never costs downtime.
func (r *Runner) checkDiskSpace() error {
	if r.cfg.MinFreeBytes == 0 {
		return nil
	}
	// Statfs needs an existing path; the staging directory itself may not
	// exist yet, so measure the data root it lives under.
	if err := os.MkdirAll(r.cfg.StagingDir, 0o700); err != nil {
		return fmt.Errorf("backup: create staging dir %q: %w", r.cfg.StagingDir, err)
	}
	free, err := r.freeBytes(r.cfg.StagingDir)
	if err != nil {
		return fmt.Errorf("backup: check free space: %w", err)
	}
	if free < r.cfg.MinFreeBytes {
		return fmt.Errorf("%w: %d bytes free, %d required", ErrInsufficientDiskSpace, free, r.cfg.MinFreeBytes)
	}
	return nil
}

// classifyExit maps a run's outcome onto an ExitReason. Context cancellation
// and agent shutdown are not distinguished: both mean "stop what you are
// doing", and ShouldRestartContainers treats them identically.
func classifyExit(ctx context.Context, err error) ExitReason {
	if err == nil {
		return ExitSuccess
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ExitTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return ExitCancelled
	default:
		return ExitFailure
	}
}

// managedVolumes returns only the Managed volumes in spec; Ephemeral volumes
// are never backed up.
func managedVolumes(spec v1.ProjectSpec) []v1.VolumeDef {
	var out []v1.VolumeDef
	for _, v := range spec.Volumes {
		if v.Type == v1.VolumeTypeManaged {
			out = append(out, v)
		}
	}
	return out
}

// freeBytesOn reports the free space available on the filesystem holding
// path.
func freeBytesOn(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
