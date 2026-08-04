package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// defaultMaxExtractedBytes bounds how far a single volume's archive may
// expand. It is a bomb guard, not a quota: a legitimate volume that genuinely
// holds this much data is already beyond what this design targets, and the
// alternative to a finite cap is letting a crafted archive fill the node's
// disk.
const defaultMaxExtractedBytes int64 = 64 << 30 // 64 GiB

// ErrNeverBackedUp is returned when a Project has no `latest.json` at all:
// nothing has ever been backed up under this name. This is a normal state for
// a new Project, and the caller may respond by initialising empty volumes when
// the spec allows it.
var ErrNeverBackedUp = errors.New("restore: project has never been backed up")

// ErrGenerationMissing is returned when a generation that should exist does
// not — most often a `latest.json` pointing at a manifest that is gone.
//
// This must never be confused with ErrNeverBackedUp. Backups demonstrably
// exist for this Project, so starting it on empty volumes would present real
// data loss as a successful restore. The correct response is to fail loudly.
var ErrGenerationMissing = errors.New("restore: backup generation is missing")

// ErrManifestMismatch is returned when a generation does not contain a volume
// the current spec declares.
var ErrManifestMismatch = errors.New("restore: generation does not match project spec")

// ErrChecksumMismatch is returned when a downloaded archive does not match the
// size or digest the manifest recorded for it.
var ErrChecksumMismatch = errors.New("restore: archive does not match manifest")

// ErrInsufficientDiskSpace is returned when the filesystem cannot be trusted
// to hold a restore. It is raised before anything is downloaded or moved, so
// a full disk costs no partially-replaced volumes.
var ErrInsufficientDiskSpace = errors.New("restore: insufficient free disk space")

// Store is the slice of objectstore.ObjectStore that restoring needs. Keeping
// it narrow makes the flow testable without a live object store.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, objectstore.ObjectMeta, error)
}

// Config holds the settings a Restorer needs from AgentConfig.
type Config struct {
	// DataRoot is the agent-owned directory Managed volumes live under.
	DataRoot string
	// MaxExtractedBytes bounds a single volume's expanded size. Zero selects
	// defaultMaxExtractedBytes.
	MaxExtractedBytes int64
	// MinFreeBytes aborts a restore before anything is downloaded if the
	// filesystem has less free space than this. A restore transiently needs
	// roughly 2-3x the volume set's size — the compressed archives, the
	// extracted copy in staging, and the previous data set aside during the
	// swap — so running out part-way is both likely and expensive. Zero
	// disables the check.
	MinFreeBytes uint64
	// Timeout bounds the object store work — resolving the manifest and
	// downloading the archives. Without it a stalled transfer leaves the
	// Project waiting to start indefinitely. Extraction is bounded by
	// MaxExtractedBytes rather than by time, since it is local disk work with
	// a known ceiling rather than a network operation that can hang. Zero
	// means no bound beyond the caller's context.
	Timeout time.Duration
}

// Restorer puts a Project's Managed volumes back on disk from a backup
// generation.
type Restorer struct {
	store  Store
	cfg    Config
	logger *zap.Logger

	// now and freeBytes are injectable so tests can control timestamps and
	// simulate a full disk.
	now       func() time.Time
	freeBytes func(path string) (uint64, error)
}

// NewRestorer creates a Restorer reading from store.
func NewRestorer(store Store, cfg Config, logger *zap.Logger) *Restorer {
	if cfg.MaxExtractedBytes == 0 {
		cfg.MaxExtractedBytes = defaultMaxExtractedBytes
	}
	return &Restorer{
		store:     store,
		cfg:       cfg,
		logger:    logger,
		now:       time.Now,
		freeBytes: freeBytesOn,
	}
}

// checkDiskSpace refuses a restore that would run the filesystem below the
// configured floor. Checked before anything is downloaded or moved, so an
// undersized disk costs no partially-replaced volumes.
func (r *Restorer) checkDiskSpace() error {
	if r.cfg.MinFreeBytes == 0 {
		return nil
	}
	// Statfs needs an existing path; the data root always exists by the time a
	// restore runs, whereas the staging directory may not.
	free, err := r.freeBytes(r.cfg.DataRoot)
	if err != nil {
		return fmt.Errorf("restore: check free space: %w", err)
	}
	if free < r.cfg.MinFreeBytes {
		return fmt.Errorf("%w: %d bytes free, %d required",
			ErrInsufficientDiskSpace, free, r.cfg.MinFreeBytes)
	}
	return nil
}

// freeBytesOn reports the free space available on the filesystem holding path.
func freeBytesOn(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// ResolveLatest reads a Project's latest.json and returns the backupID it
// points at.
//
// This is deliberately separate from RestoreGeneration. The core restore
// operation names an explicit generation; "which one is newest" is a question
// answered before it, exactly as `restic restore latest` resolves the word
// latest to a snapshot ID before restoring. Keeping the two apart is what
// lets an operator-initiated rollback reuse the whole restore path by simply
// supplying a different backupID.
//
// Returns ErrNeverBackedUp when the Project has no latest.json at all.
func (r *Restorer) ResolveLatest(ctx context.Context, namespace, project string) (string, error) {
	key, err := backup.LatestKey(namespace, project)
	if err != nil {
		return "", err
	}

	body, _, err := r.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return "", ErrNeverBackedUp
		}
		return "", fmt.Errorf("restore: read latest pointer: %w", err)
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("restore: read latest pointer: %w", err)
	}

	latest, err := backup.UnmarshalLatest(raw)
	if err != nil {
		return "", err
	}
	if latest.BackupID == "" {
		return "", fmt.Errorf("restore: latest pointer for %s/%s names no generation", namespace, project)
	}
	return latest.BackupID, nil
}

// FetchManifest reads one generation's manifest.
//
// A missing manifest returns ErrGenerationMissing, never ErrNeverBackedUp:
// reaching this function means something already named a generation, so its
// absence is a fault to surface rather than grounds to start empty.
func (r *Restorer) FetchManifest(ctx context.Context, namespace, project, backupID string) (backup.Manifest, error) {
	key, err := backup.ManifestKey(namespace, project, backupID)
	if err != nil {
		return backup.Manifest{}, err
	}

	body, _, err := r.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return backup.Manifest{}, fmt.Errorf("%w: generation %q has no manifest", ErrGenerationMissing, backupID)
		}
		return backup.Manifest{}, fmt.Errorf("restore: read manifest for %q: %w", backupID, err)
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(body)
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("restore: read manifest for %q: %w", backupID, err)
	}
	return backup.UnmarshalManifest(raw)
}

// RestoreGeneration puts every Managed volume the Project declares back on
// disk from the named generation, then records the placement in a marker.
//
// The ordering is what makes a generation land as a unit:
//
//  1. download every archive and verify it against the manifest — nothing is
//     extracted until every archive is known good, so a corrupt download
//     cannot leave half the volumes replaced
//  2. extract every archive into staging
//  3. only then swap the staged directories into place, rolling back the
//     already-swapped ones if any swap fails
//
// backupID is explicit rather than implied. Callers restoring the newest
// generation resolve it with ResolveLatest first.
func (r *Restorer) RestoreGeneration(ctx context.Context, project *v1.Project, backupID string) error {
	namespace, name := project.Namespace, project.Name
	log := r.logger.With(
		zap.String("project", namespace+"/"+name),
		zap.String("backupID", backupID),
	)

	// Checked first, while nothing has been downloaded and no live data has
	// been touched: a full disk should cost a skipped restore, not a Project
	// left with some volumes replaced and others not.
	if err := r.checkDiskSpace(); err != nil {
		return err
	}

	if r.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
	}

	manifest, err := r.FetchManifest(ctx, namespace, name, backupID)
	if err != nil {
		return err
	}

	wanted, err := matchVolumes(project.Spec.Volumes, manifest, log)
	if err != nil {
		return err
	}
	if len(wanted) == 0 {
		log.Info("Generation contains no volumes this spec declares; nothing to restore")
		return nil
	}

	staging, err := StagingDir(r.cfg.DataRoot, namespace, name)
	if err != nil {
		return err
	}
	// Residue from a previous attempt is worthless — that process is gone.
	// The decision to restore has already been taken by this point, so
	// clearing it now cannot erase a signal anything still needs.
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("restore: clear staging %q: %w", staging, err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return fmt.Errorf("restore: create staging %q: %w", staging, err)
	}
	// Deliberately not deferred. Staging is removed only on success, because
	// its presence is what tells the next startup that the volumes cannot be
	// trusted. Deleting it on a failed restore would erase that evidence and
	// let a half-swapped volume set be adopted as legitimate on the next pass.

	// Pass 1: download and verify everything before touching any live data.
	archives := make(map[string]string, len(wanted))
	for _, entry := range wanted {
		archivePath := filepath.Join(staging, entry.Name+".tar.gz")
		if err := r.downloadAndVerify(ctx, entry, archivePath); err != nil {
			return err
		}
		archives[entry.Name] = archivePath
		log.Debug("Archive verified", zap.String("volume", entry.Name))
	}

	// Pass 2: expand into staging, still without touching live data.
	staged := make(map[string]string, len(wanted))
	for _, entry := range wanted {
		dir := filepath.Join(staging, "extracted", entry.Name)
		if err := Extract(archives[entry.Name], dir, r.cfg.MaxExtractedBytes); err != nil {
			return err
		}
		staged[entry.Name] = dir
	}

	// Pass 3: swap. Only now can live data change.
	if err := r.swapAll(namespace, name, staged, log); err != nil {
		return err
	}

	if err := WriteMarker(r.cfg.DataRoot, namespace, name, backupID, r.now()); err != nil {
		return err
	}

	// Success: the volumes are whole again, so the "do not trust this disk"
	// signal can go.
	if err := os.RemoveAll(staging); err != nil {
		log.Warn("Failed to remove staging directory",
			zap.String("path", staging), zap.Error(err))
	}

	log.Info("Restored generation", zap.Int("volumes", len(wanted)))
	return nil
}

// InitializeEmpty creates empty host directories for a Project's Managed
// volumes and records the placement, for the case where no generation exists
// yet and the spec allows starting fresh.
//
// The marker matters as much here as after a real restore: without it, the
// next agent restart would see empty directories, conclude this is a new
// placement, and restore over whatever the containers have written since.
func (r *Restorer) InitializeEmpty(project *v1.Project) error {
	namespace, name := project.Namespace, project.Name

	for _, vol := range project.Spec.Volumes {
		if vol.Type != v1.VolumeTypeManaged {
			continue
		}
		path, err := caravolume.HostPath(r.cfg.DataRoot, namespace, name, vol.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("restore: create empty volume dir %q: %w", path, err)
		}
	}

	if err := WriteMarker(r.cfg.DataRoot, namespace, name, "", r.now()); err != nil {
		return err
	}

	// This path is reachable with staging still on disk: a restore died
	// mid-flight and the generation it was fetching later went missing, so the
	// next pass resolves "never backed up" instead. Leaving staging would turn
	// a one-off crash signal into a permanent one — Decide ranks staging above
	// the marker, so every future pass would restore, and the first generation
	// written after this point would overwrite whatever the containers have
	// produced in the meantime. The volumes are established now, so the signal
	// has served its purpose.
	staging, err := StagingDir(r.cfg.DataRoot, namespace, name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("restore: clear staging %q: %w", staging, err)
	}
	return nil
}

// matchVolumes reconciles the generation against the current spec.
//
// A Managed volume the spec declares but the generation lacks is fatal:
// starting it empty would look like a successful restore while silently
// losing that volume's data. A volume the generation holds but the spec no
// longer declares is simply skipped — the Project legitimately dropped it.
func matchVolumes(specVolumes []v1.VolumeDef, manifest backup.Manifest, log *zap.Logger) ([]backup.VolumeManifestEntry, error) {
	inGeneration := make(map[string]backup.VolumeManifestEntry, len(manifest.Volumes))
	for _, entry := range manifest.Volumes {
		inGeneration[entry.Name] = entry
	}

	declared := make(map[string]bool, len(specVolumes))
	var wanted []backup.VolumeManifestEntry

	for _, vol := range specVolumes {
		if vol.Type != v1.VolumeTypeManaged {
			continue
		}
		declared[vol.Name] = true

		entry, ok := inGeneration[vol.Name]
		if !ok {
			return nil, fmt.Errorf("%w: generation %q has no volume %q",
				ErrManifestMismatch, manifest.BackupID, vol.Name)
		}
		wanted = append(wanted, entry)
	}

	for _, entry := range manifest.Volumes {
		if !declared[entry.Name] {
			log.Info("Ignoring volume present in generation but no longer in spec",
				zap.String("volume", entry.Name))
		}
	}
	return wanted, nil
}

// downloadAndVerify streams one archive to disk, hashing as it goes, and
// checks both size and digest against what the manifest recorded.
//
// Verification happens before any extraction so a truncated or corrupted
// download can never reach live volume data.
func (r *Restorer) downloadAndVerify(ctx context.Context, entry backup.VolumeManifestEntry, destPath string) error {
	body, _, err := r.store.Get(ctx, entry.ArchiveKey)
	if err != nil {
		return fmt.Errorf("restore: download %q: %w", entry.ArchiveKey, err)
	}
	defer func() { _ = body.Close() }()

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("restore: create %q: %w", destPath, err)
	}
	defer func() { _ = out.Close() }()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, hasher), body)
	if err != nil {
		return fmt.Errorf("restore: download %q: %w", entry.ArchiveKey, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("restore: sync %q: %w", destPath, err)
	}

	if written != entry.SizeBytes {
		return fmt.Errorf("%w: volume %q downloaded %d bytes, manifest says %d",
			ErrChecksumMismatch, entry.Name, written, entry.SizeBytes)
	}
	if digest := hex.EncodeToString(hasher.Sum(nil)); digest != entry.SHA256 {
		return fmt.Errorf("%w: volume %q digest %s, manifest says %s",
			ErrChecksumMismatch, entry.Name, digest, entry.SHA256)
	}
	return nil
}

// swapAll moves every staged directory into its live location, undoing the
// ones already moved if a later one fails.
//
// Multi-directory atomicity is not something POSIX offers, so this narrows
// the window instead: every rename is on the same filesystem and therefore
// near-instantaneous, and a failure part-way is rolled back rather than left
// as a Project split across two generations.
func (r *Restorer) swapAll(namespace, project string, staged map[string]string, log *zap.Logger) error {
	type swap struct {
		live      string
		displaced string // where the previous contents were moved, empty if none
	}
	done := make([]swap, 0, len(staged))

	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			s := done[i]
			if err := os.RemoveAll(s.live); err != nil {
				log.Error("Rollback: failed to remove restored data",
					zap.String("path", s.live), zap.Error(err))
				continue
			}
			if s.displaced == "" {
				continue
			}
			if err := os.Rename(s.displaced, s.live); err != nil {
				log.Error("Rollback: failed to restore previous data",
					zap.String("path", s.live), zap.Error(err))
			}
		}
	}

	for volume, stagedDir := range staged {
		live, err := caravolume.HostPath(r.cfg.DataRoot, namespace, project, volume)
		if err != nil {
			rollback()
			return err
		}
		if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
			rollback()
			return fmt.Errorf("restore: create volume dir for %q: %w", volume, err)
		}

		var displaced string
		if _, err := os.Stat(live); err == nil {
			displaced = live + displacedSuffix
			_ = os.RemoveAll(displaced)
			if err := os.Rename(live, displaced); err != nil {
				rollback()
				return fmt.Errorf("restore: set aside existing data for %q: %w", volume, err)
			}
		} else if !os.IsNotExist(err) {
			rollback()
			return fmt.Errorf("restore: inspect %q: %w", live, err)
		}

		if err := os.Rename(stagedDir, live); err != nil {
			if displaced != "" {
				_ = os.Rename(displaced, live)
			}
			rollback()
			return fmt.Errorf("restore: swap in %q: %w", volume, err)
		}
		done = append(done, swap{live: live, displaced: displaced})
	}

	// Every volume landed; the displaced copies are now dead weight.
	for _, s := range done {
		if s.displaced == "" {
			continue
		}
		if err := os.RemoveAll(s.displaced); err != nil {
			log.Warn("Failed to remove displaced data",
				zap.String("path", s.displaced), zap.Error(err))
		}
	}
	return nil
}
