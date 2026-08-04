package restore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeStore serves objects from memory and can be told to fail or corrupt
// specific keys.
type fakeStore struct {
	objects   map[string][]byte
	failOnKey string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (f *fakeStore) Get(ctx context.Context, key string) (io.ReadCloser, objectstore.ObjectMeta, error) {
	// A real object store client honours the context; the fake must too, or
	// timeout behaviour is untestable here.
	if err := ctx.Err(); err != nil {
		return nil, objectstore.ObjectMeta{}, err
	}
	if key == f.failOnKey {
		return nil, objectstore.ObjectMeta{}, io.ErrUnexpectedEOF
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, objectstore.ObjectMeta{}, objectstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), objectstore.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

// makeArchive builds a one-file tar.gz and returns its bytes, size and digest.
func makeArchive(t *testing.T, filename, content string) (data []byte, size int64, digest string) {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: filename, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content)),
	}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	data = buf.Bytes()
	sum := sha256.Sum256(data)
	return data, int64(len(data)), hex.EncodeToString(sum[:])
}

// harness wires a Restorer over a fake store, and seeds one generation.
type harness struct {
	restorer *Restorer
	store    *fakeStore
	dataRoot string
	project  *v1.Project
	backupID string
}

func newHarness(t *testing.T, volumes ...string) *harness {
	t.Helper()

	dataRoot := t.TempDir()
	store := newFakeStore()
	const backupID = "20260730T120000Z-abcd1234"

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
			Backup:   &v1.ProjectBackupConfig{Interval: "168h"},
		},
	}

	var entries []backup.VolumeManifestEntry
	for _, name := range volumes {
		project.Spec.Volumes = append(project.Spec.Volumes,
			v1.VolumeDef{Name: name, Type: v1.VolumeTypeManaged})

		data, size, digest := makeArchive(t, "content.txt", "data for "+name)
		key, err := backup.ArchiveKey("default", "blog", backupID, name)
		require.NoError(t, err)
		store.objects[key] = data

		entries = append(entries, backup.VolumeManifestEntry{
			Name: name, ArchiveKey: key, SizeBytes: size, SHA256: digest,
		})
	}

	manifest := backup.BuildManifest(backupID, "default", "blog", "node-a", "test", 7,
		time.Now(), entries)
	manifestBytes, err := manifest.Marshal()
	require.NoError(t, err)
	manifestKey, err := backup.ManifestKey("default", "blog", backupID)
	require.NoError(t, err)
	store.objects[manifestKey] = manifestBytes

	latestBytes, err := backup.NewLatest(backupID, manifestKey, time.Now()).Marshal()
	require.NoError(t, err)
	latestKey, err := backup.LatestKey("default", "blog")
	require.NoError(t, err)
	store.objects[latestKey] = latestBytes

	return &harness{
		restorer: NewRestorer(store, Config{DataRoot: dataRoot}, zap.NewNop()),
		store:    store,
		dataRoot: dataRoot,
		project:  project,
		backupID: backupID,
	}
}

func (h *harness) volumeFile(volume, file string) string {
	return filepath.Join(h.dataRoot, "volumes", "default", "blog", volume, "data", file)
}

// ── ResolveLatest ────────────────────────────────────────────────────────────

func TestResolveLatest(t *testing.T) {
	h := newHarness(t, "db-data")

	got, err := h.restorer.ResolveLatest(context.Background(), "default", "blog")
	require.NoError(t, err)
	assert.Equal(t, h.backupID, got)
}

func TestResolveLatestWithNoGeneration(t *testing.T) {
	// A Project that has never been backed up is a normal state, not a fault —
	// the caller turns it into "initialize empty" when the spec allows.
	h := newHarness(t, "db-data")
	latestKey, err := backup.LatestKey("default", "blog")
	require.NoError(t, err)
	delete(h.store.objects, latestKey)

	_, err = h.restorer.ResolveLatest(context.Background(), "default", "blog")
	assert.ErrorIs(t, err, ErrNeverBackedUp)
}

func TestResolveLatestIsSeparateFromRestore(t *testing.T) {
	// The property Correction 3 exists to preserve: a caller can restore a
	// generation it names itself, without ResolveLatest being involved. This
	// is what makes operator-initiated rollback a thin addition later.
	h := newHarness(t, "db-data")

	// Deleting latest.json makes "newest" unresolvable, yet an explicitly
	// named generation still restores.
	latestKey, err := backup.LatestKey("default", "blog")
	require.NoError(t, err)
	delete(h.store.objects, latestKey)

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	got, err := os.ReadFile(h.volumeFile("db-data", "content.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data for db-data", string(got))
}

// ── RestoreGeneration ────────────────────────────────────────────────────────

func TestRestoreGenerationRestoresEveryVolume(t *testing.T) {
	h := newHarness(t, "db-data", "uploads")

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	for _, vol := range []string{"db-data", "uploads"} {
		got, err := os.ReadFile(h.volumeFile(vol, "content.txt"))
		require.NoError(t, err, "volume %q should have been restored", vol)
		assert.Equal(t, "data for "+vol, string(got))
	}
}

func TestRestoreGenerationWritesMarker(t *testing.T) {
	h := newHarness(t, "db-data")

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	m, err := ReadMarker(h.dataRoot, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Equal(t, h.backupID, m.BackupID,
		"the marker records which generation this node holds")
}

func TestRestoreGenerationReplacesExistingData(t *testing.T) {
	// The manual-rollback path will rely on this: when a restore does run, it
	// replaces whatever was there rather than merging into it.
	h := newHarness(t, "db-data")

	live := filepath.Dir(h.volumeFile("db-data", "x"))
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "stale.txt"), []byte("old"), 0o600))

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	_, err := os.Stat(filepath.Join(live, "stale.txt"))
	assert.True(t, os.IsNotExist(err), "files absent from the generation must not survive")

	got, err := os.ReadFile(h.volumeFile("db-data", "content.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data for db-data", string(got))
}

func TestRestoreGenerationRejectsChecksumMismatch(t *testing.T) {
	// A truncated or corrupted download must never reach live volume data.
	h := newHarness(t, "db-data")

	key, err := backup.ArchiveKey("default", "blog", h.backupID, "db-data")
	require.NoError(t, err)
	h.store.objects[key] = append(h.store.objects[key], 0x00)

	err = h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)

	assert.ErrorIs(t, err, ErrChecksumMismatch)
	_, statErr := os.Stat(h.volumeFile("db-data", "content.txt"))
	assert.True(t, os.IsNotExist(statErr), "nothing may be written when verification fails")
}

func TestRestoreGenerationVerifiesBeforeTouchingAnyVolume(t *testing.T) {
	// Two volumes, the second corrupt. Because every archive is verified
	// before any extraction begins, the first volume must be left untouched
	// rather than replaced and then abandoned half-way.
	h := newHarness(t, "db-data", "uploads")

	live := filepath.Dir(h.volumeFile("db-data", "x"))
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "original.txt"), []byte("keep me"), 0o600))

	key, err := backup.ArchiveKey("default", "blog", h.backupID, "uploads")
	require.NoError(t, err)
	h.store.objects[key] = []byte("not a valid archive")

	err = h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)
	require.Error(t, err)

	got, readErr := os.ReadFile(filepath.Join(live, "original.txt"))
	require.NoError(t, readErr, "the healthy volume must not have been replaced")
	assert.Equal(t, "keep me", string(got))
}

func TestRestoreGenerationFailsWhenSpecVolumeMissingFromGeneration(t *testing.T) {
	// Starting a declared volume empty would look like a successful restore
	// while silently losing that volume's data.
	h := newHarness(t, "db-data")
	h.project.Spec.Volumes = append(h.project.Spec.Volumes,
		v1.VolumeDef{Name: "uploads", Type: v1.VolumeTypeManaged})

	err := h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)

	assert.ErrorIs(t, err, ErrManifestMismatch)
	_, statErr := os.Stat(h.volumeFile("db-data", "content.txt"))
	assert.True(t, os.IsNotExist(statErr), "no volume may be restored when the set does not match")
}

func TestRestoreGenerationIgnoresVolumeDroppedFromSpec(t *testing.T) {
	// The opposite direction is benign: the Project legitimately removed a
	// volume, so the generation simply carries one we no longer want.
	h := newHarness(t, "db-data", "uploads")
	h.project.Spec.Volumes = []v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}}

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	got, err := os.ReadFile(h.volumeFile("db-data", "content.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data for db-data", string(got))

	_, statErr := os.Stat(h.volumeFile("uploads", "content.txt"))
	assert.True(t, os.IsNotExist(statErr), "a volume the spec dropped must not be restored")
}

func TestRestoreGenerationIgnoresEphemeralVolumes(t *testing.T) {
	h := newHarness(t, "db-data")
	h.project.Spec.Volumes = append(h.project.Spec.Volumes,
		v1.VolumeDef{Name: "cache", Type: v1.VolumeTypeEphemeral})

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	_, statErr := os.Stat(h.volumeFile("cache", "content.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestRestoreGenerationWithUnknownGeneration(t *testing.T) {
	h := newHarness(t, "db-data")

	err := h.restorer.RestoreGeneration(context.Background(), h.project, "does-not-exist")
	assert.ErrorIs(t, err, ErrGenerationMissing)
}

func TestRestoreGenerationCleansStaging(t *testing.T) {
	h := newHarness(t, "db-data")

	require.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	staging, err := StagingDir(h.dataRoot, "default", "blog")
	require.NoError(t, err)
	_, statErr := os.Stat(staging)
	assert.True(t, os.IsNotExist(statErr), "staging must not survive a successful restore")
}

func TestRestoreGenerationKeepsStagingAfterFailure(t *testing.T) {
	// Deliberately the opposite of the success case. Staging is the signal
	// that this Project's volumes cannot be trusted; removing it after a
	// failed restore would erase that, and the next pass would see no marker,
	// data on disk, and conclude "adopt" — blessing a possibly half-swapped
	// volume set as legitimate.
	h := newHarness(t, "db-data")
	key, err := backup.ArchiveKey("default", "blog", h.backupID, "db-data")
	require.NoError(t, err)
	h.store.objects[key] = append(h.store.objects[key], 0x00)

	require.Error(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))

	present, err := StagingPresent(h.dataRoot, "default", "blog")
	require.NoError(t, err)
	assert.True(t, present, "a failed restore must leave the signal in place")

	// And that signal must drive the next decision back to Restore.
	assert.Equal(t, DecisionRestore, Decide(present, false, true))
}

// ── InitializeEmpty ──────────────────────────────────────────────────────────

func TestInitializeEmptyCreatesDirsAndMarker(t *testing.T) {
	h := newHarness(t, "db-data", "uploads")

	require.NoError(t, h.restorer.InitializeEmpty(h.project))

	for _, vol := range []string{"db-data", "uploads"} {
		info, err := os.Stat(filepath.Dir(h.volumeFile(vol, "x")))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	}

	m, err := ReadMarker(h.dataRoot, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.Empty(t, m.BackupID, "no generation was restored, so none is recorded")
}

func TestInitializeEmptyMarkerPreventsLaterOverwrite(t *testing.T) {
	// The reason InitializeEmpty writes a marker at all: without it, the next
	// restart would see empty directories, call this a fresh placement, and
	// restore over whatever the containers wrote in the meantime.
	h := newHarness(t, "db-data")

	require.NoError(t, h.restorer.InitializeEmpty(h.project))

	m, err := ReadMarker(h.dataRoot, "default", "blog")
	require.NoError(t, err)
	require.NotNil(t, m)

	assert.Equal(t, DecisionSkip, Decide(false, m != nil, false))
}

// ── Correction 5: the two "no generation" failures must not be conflated ─────

func TestNeverBackedUpAndGenerationMissingAreDistinguishable(t *testing.T) {
	// This is the whole point of having two sentinels. "Never backed up" may
	// legitimately start empty volumes; "the generation is gone" must not,
	// because real backups exist and starting empty would present data loss as
	// a successful restore.
	t.Run("no latest.json means never backed up", func(t *testing.T) {
		h := newHarness(t, "db-data")
		latestKey, err := backup.LatestKey("default", "blog")
		require.NoError(t, err)
		delete(h.store.objects, latestKey)

		_, err = h.restorer.ResolveLatest(context.Background(), "default", "blog")

		assert.ErrorIs(t, err, ErrNeverBackedUp)
		assert.NotErrorIs(t, err, ErrGenerationMissing)
	})

	t.Run("dangling latest.json means the generation is missing", func(t *testing.T) {
		// latest.json survives, but the manifest it names is gone — deleted by
		// hand, lost to a future retention policy, or an object store fault.
		h := newHarness(t, "db-data")
		manifestKey, err := backup.ManifestKey("default", "blog", h.backupID)
		require.NoError(t, err)
		delete(h.store.objects, manifestKey)

		resolved, err := h.restorer.ResolveLatest(context.Background(), "default", "blog")
		require.NoError(t, err, "the pointer itself still reads fine")

		_, err = h.restorer.FetchManifest(context.Background(), "default", "blog", resolved)

		assert.ErrorIs(t, err, ErrGenerationMissing)
		assert.NotErrorIs(t, err, ErrNeverBackedUp,
			"a project with real backups must never be treated as never-backed-up")
	})
}

func TestRestoreGenerationFailsOnDanglingPointer(t *testing.T) {
	h := newHarness(t, "db-data")
	manifestKey, err := backup.ManifestKey("default", "blog", h.backupID)
	require.NoError(t, err)
	delete(h.store.objects, manifestKey)

	err = h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)

	assert.ErrorIs(t, err, ErrGenerationMissing)
	_, statErr := os.Stat(h.volumeFile("db-data", "content.txt"))
	assert.True(t, os.IsNotExist(statErr), "no volume may be written when the generation is gone")
}

// ── Preconditions ────────────────────────────────────────────────────────────

func TestRestoreAbortsWhenDiskIsFull(t *testing.T) {
	// Checked before anything is downloaded, so a full disk costs a skipped
	// restore rather than a half-replaced volume set.
	h := newHarness(t, "db-data")
	h.restorer.cfg.MinFreeBytes = 1 << 40
	h.restorer.freeBytes = func(string) (uint64, error) { return 1024, nil }

	err := h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)

	assert.ErrorIs(t, err, ErrInsufficientDiskSpace)
	staging, sErr := StagingDir(h.dataRoot, "default", "blog")
	require.NoError(t, sErr)
	_, statErr := os.Stat(staging)
	assert.True(t, os.IsNotExist(statErr), "nothing may be staged when the precondition fails")
}

func TestRestoreProceedsWhenDiskCheckDisabled(t *testing.T) {
	h := newHarness(t, "db-data")
	h.restorer.cfg.MinFreeBytes = 0 // disabled
	h.restorer.freeBytes = func(string) (uint64, error) { return 0, nil }

	assert.NoError(t, h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID))
}

func TestRestoreRespectsTimeout(t *testing.T) {
	h := newHarness(t, "db-data")
	h.restorer.cfg.Timeout = time.Nanosecond

	err := h.restorer.RestoreGeneration(context.Background(), h.project, h.backupID)
	require.Error(t, err, "an expired deadline must surface rather than hang")
}

// TestInitializeEmptyClearsStaging covers the case where an interrupted
// restore and a vanished generation coincide.
//
// Staging outranks the marker in Decide, so leaving it here would turn a
// one-off crash signal into a permanent one: every later pass would restore,
// and the first generation written after this point would overwrite whatever
// the containers produced in the meantime.
func TestInitializeEmptyClearsStaging(t *testing.T) {
	dataRoot := t.TempDir()
	r := NewRestorer(newFakeStore(), Config{DataRoot: dataRoot}, zap.NewNop())
	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default"},
		Spec: v1.ProjectSpec{
			Volumes: []v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}},
		},
	}

	staging, err := StagingDir(dataRoot, project.Namespace, project.Name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(staging, 0o700))

	require.NoError(t, r.InitializeEmpty(project))

	present, err := StagingPresent(dataRoot, project.Namespace, project.Name)
	require.NoError(t, err)
	assert.False(t, present, "an established placement must not keep the distrust signal")

	marker, err := ReadMarker(dataRoot, project.Namespace, project.Name)
	require.NoError(t, err)
	require.NotNil(t, marker)
	assert.Empty(t, marker.BackupID, "empty volumes came from no generation")
}
