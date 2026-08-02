//go:build e2e

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"NYCU-SDC/caravanserai/internal/agent/restore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// volumeDataDir is where a Managed volume's contents live on disk. Both the
// backup and the restore derive it independently; spelling it out here keeps
// the test honest about what it is actually checking.
func volumeDataDir(dataRoot, namespace, project, volume string) string {
	return filepath.Join(dataRoot, "volumes", namespace, project, volume, "data")
}

func writeVolumeFile(t *testing.T, dataRoot, project, volume, name, contents string) {
	t.Helper()
	dir := volumeDataDir(dataRoot, "default", project, volume)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600))
}

func readVolumeFile(t *testing.T, dataRoot, project, volume, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(volumeDataDir(dataRoot, "default", project, volume), name))
	require.NoError(t, err)
	return string(data)
}

// newRestorer builds a Restorer reading from the same MinIO container the
// runner writes to. A restore that cannot read what the backup wrote is the
// whole failure mode these tests exist to catch.
func newRestorer(t *testing.T, dataRoot string) *restore.Restorer {
	t.Helper()
	return restore.NewRestorer(newStore(t), restore.Config{DataRoot: dataRoot}, zap.NewNop())
}

// TestBackupRestoreRoundTrip is the claim the whole feature rests on: bytes
// written by a real backup, through real gzip and a real S3 server, come back
// byte-identical after the local copy is destroyed.
//
// The unit tests fake the object store, so this is the only place the tar.gz
// produced by the backup is ever fed to the extractor that must read it.
func TestBackupRestoreRoundTrip(t *testing.T) {
	h := newRunnerHarness(t, "e2e-roundtrip", "db-data", "uploads")
	ctx := context.Background()

	writeVolumeFile(t, h.dataRoot, "e2e-roundtrip", "db-data", "rows.txt", "row-1\nrow-2\n")
	writeVolumeFile(t, h.dataRoot, "e2e-roundtrip", "uploads", "photo.bin", "\x00\x01\x02binary\xff")

	require.NoError(t, h.runner.Run(ctx, h.project))

	// Destroy the local copy entirely — a restore that quietly reuses
	// surviving local data would pass a weaker test.
	require.NoError(t, os.RemoveAll(filepath.Join(h.dataRoot, "volumes", "default", "e2e-roundtrip")))

	restorer := newRestorer(t, h.dataRoot)
	backupID, err := restorer.ResolveLatest(ctx, "default", "e2e-roundtrip")
	require.NoError(t, err)
	require.NotEmpty(t, backupID)

	require.NoError(t, restorer.RestoreGeneration(ctx, h.project, backupID))

	assert.Equal(t, "row-1\nrow-2\n", readVolumeFile(t, h.dataRoot, "e2e-roundtrip", "db-data", "rows.txt"))
	assert.Equal(t, "\x00\x01\x02binary\xff", readVolumeFile(t, h.dataRoot, "e2e-roundtrip", "uploads", "photo.bin"))

	// The marker names the generation, which is what makes the next startup
	// skip instead of restoring over live writes.
	marker, err := restore.ReadMarker(h.dataRoot, "default", "e2e-roundtrip")
	require.NoError(t, err)
	require.NotNil(t, marker)
	assert.Equal(t, backupID, marker.BackupID)

	// Staging is removed only on success, so its absence is the signal that
	// this restore completed.
	present, err := restore.StagingPresent(h.dataRoot, "default", "e2e-roundtrip")
	require.NoError(t, err)
	assert.False(t, present)
}

// TestRestoreReplacesExistingVolumeData covers the swap rather than a fresh
// placement: files present before the restore and absent from the generation
// must be gone afterwards, or a restore would silently merge two points in
// time.
func TestRestoreReplacesExistingVolumeData(t *testing.T) {
	h := newRunnerHarness(t, "e2e-replace", "db-data")
	ctx := context.Background()

	writeVolumeFile(t, h.dataRoot, "e2e-replace", "db-data", "kept.txt", "from the generation")
	require.NoError(t, h.runner.Run(ctx, h.project))

	// Write after the backup: this file exists in no generation.
	writeVolumeFile(t, h.dataRoot, "e2e-replace", "db-data", "later.txt", "written after the backup")

	restorer := newRestorer(t, h.dataRoot)
	backupID, err := restorer.ResolveLatest(ctx, "default", "e2e-replace")
	require.NoError(t, err)
	require.NoError(t, restorer.RestoreGeneration(ctx, h.project, backupID))

	assert.Equal(t, "from the generation", readVolumeFile(t, h.dataRoot, "e2e-replace", "db-data", "kept.txt"))

	_, statErr := os.Stat(filepath.Join(
		volumeDataDir(h.dataRoot, "default", "e2e-replace", "db-data"), "later.txt"))
	assert.True(t, os.IsNotExist(statErr),
		"restoring a generation must replace the volume, not merge into it")
}

// TestRestoreToOlderGeneration is the operator rollback path: two generations
// exist, and naming the older one explicitly must bring back its contents even
// though a newer one is what latest.json points at.
func TestRestoreToOlderGeneration(t *testing.T) {
	h := newRunnerHarness(t, "e2e-rollback", "db-data")
	ctx := context.Background()
	restorer := newRestorer(t, h.dataRoot)

	writeVolumeFile(t, h.dataRoot, "e2e-rollback", "db-data", "state.txt", "generation one")
	require.NoError(t, h.runner.Run(ctx, h.project))
	first, err := restorer.ResolveLatest(ctx, "default", "e2e-rollback")
	require.NoError(t, err)

	writeVolumeFile(t, h.dataRoot, "e2e-rollback", "db-data", "state.txt", "generation two")
	require.NoError(t, h.runner.Run(ctx, h.project))
	second, err := restorer.ResolveLatest(ctx, "default", "e2e-rollback")
	require.NoError(t, err)
	require.NotEqual(t, first, second, "the second run must produce a distinct generation")

	require.NoError(t, restorer.RestoreGeneration(ctx, h.project, first))
	assert.Equal(t, "generation one", readVolumeFile(t, h.dataRoot, "e2e-rollback", "db-data", "state.txt"))

	// latest.json is untouched by a restore: rolling back does not rewrite
	// history, so the next backup still builds on the newest generation.
	stillLatest, err := restorer.ResolveLatest(ctx, "default", "e2e-rollback")
	require.NoError(t, err)
	assert.Equal(t, second, stillLatest)
}

// TestResolveLatestOnUnknownProject distinguishes the two absences that must
// never be confused: a Project with no backups at all may start empty, while
// a named generation that has gone missing must fail loudly.
func TestResolveLatestOnUnknownProject(t *testing.T) {
	restorer := newRestorer(t, t.TempDir())
	ctx := context.Background()

	_, err := restorer.ResolveLatest(ctx, "default", "e2e-never-backed-up")
	assert.ErrorIs(t, err, restore.ErrNeverBackedUp)

	_, err = restorer.FetchManifest(ctx, "default", "e2e-never-backed-up", "20260101T000000Z")
	assert.ErrorIs(t, err, restore.ErrGenerationMissing)
}

// TestRestoreDetectsCorruptedArchive proves the checksum in the manifest is
// actually enforced against the bytes in the bucket, and that a bad archive
// costs nothing: the live volume is untouched because verification happens
// before anything is extracted or swapped.
func TestRestoreDetectsCorruptedArchive(t *testing.T) {
	h := newRunnerHarness(t, "e2e-corrupt", "db-data")
	ctx := context.Background()

	writeVolumeFile(t, h.dataRoot, "e2e-corrupt", "db-data", "state.txt", "original")
	require.NoError(t, h.runner.Run(ctx, h.project))

	restorer := newRestorer(t, h.dataRoot)
	backupID, err := restorer.ResolveLatest(ctx, "default", "e2e-corrupt")
	require.NoError(t, err)

	manifest, err := restorer.FetchManifest(ctx, "default", "e2e-corrupt", backupID)
	require.NoError(t, err)
	require.Len(t, manifest.Volumes, 1)

	// Overwrite the archive in the bucket with something that is not what the
	// manifest recorded.
	put(t, h.store, manifest.Volumes[0].ArchiveKey, []byte("not a tar.gz"))

	writeVolumeFile(t, h.dataRoot, "e2e-corrupt", "db-data", "state.txt", "live data")

	err = restorer.RestoreGeneration(ctx, h.project, backupID)
	require.Error(t, err)
	assert.ErrorIs(t, err, restore.ErrChecksumMismatch)

	assert.Equal(t, "live data", readVolumeFile(t, h.dataRoot, "e2e-corrupt", "db-data", "state.txt"),
		"a failed verification must not have touched live data")

	// Staging survives a failed restore. That is the signal telling the next
	// startup this Project's volumes cannot be trusted.
	present, err := restore.StagingPresent(h.dataRoot, "default", "e2e-corrupt")
	require.NoError(t, err)
	assert.True(t, present)
}
