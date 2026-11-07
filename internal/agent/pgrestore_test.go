package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackrest"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeRestoreRuntime struct {
	*pgFakeRuntime
	mu    sync.Mutex
	specs []docker.OneShotSpec
	run   func(ctx context.Context, spec docker.OneShotSpec) (docker.ExecResult, error)
}

func (f *fakeRestoreRuntime) RunToCompletion(context.Context, string, string, []string, []string) (docker.ExecResult, error) {
	return docker.ExecResult{}, nil
}

func (f *fakeRestoreRuntime) RemoveContainer(context.Context, string) error { return nil }

func (f *fakeRestoreRuntime) RunOneShot(ctx context.Context, spec docker.OneShotSpec) (docker.ExecResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	return f.run(ctx, spec)
}

type fixedOwnership struct {
	mu     sync.Mutex
	result backup.Ownership
}

func (o *fixedOwnership) set(v backup.Ownership) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.result = v
}

func (o *fixedOwnership) Resolve(context.Context, backup.ResourceKey, string, int64) backup.Ownership {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.result
}

type restoreHarness struct {
	restores  *pgRestores
	runtime   *fakeRestoreRuntime
	ownership *fixedOwnership
	dataRoot  string
	project   *v1.Project
	reported  []string
	gave      []string
	clock     time.Time
}

func newRestoreHarness(t *testing.T) *restoreHarness {
	t.Helper()
	dataRoot := t.TempDir()
	h := &restoreHarness{
		dataRoot:  dataRoot,
		ownership: &fixedOwnership{},
		project:   pgProject(),
		clock:     time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
	h.project.Status.AssignmentGeneration = 2
	h.project.Status.Backup = &v1.ProjectBackupStatus{Completions: []v1.BackupCompletion{
		{BackupID: "old", Items: []v1.BackupCompletionItem{{Volume: "pgdata", Label: "old-label"}}},
		{BackupID: "new", Items: []v1.BackupCompletionItem{{Volume: "pgdata", Label: "new-label"}}},
	}}
	h.runtime = &fakeRestoreRuntime{pgFakeRuntime: newPgFakeRuntime()}
	h.runtime.run = func(_ context.Context, spec docker.OneShotSpec) (docker.ExecResult, error) {
		dir := strings.SplitN(spec.Binds[0], ":", 2)[0]
		require.NoError(t, os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16"), 0o600))
		return docker.ExecResult{Stdout: "last-transaction=2026-11-01 11:59:00 UTC\n"}, nil
	}
	restorer := restore.NewRestorer(nil, restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop())
	h.restores = newPGRestores(h.runtime, &fakeRepo{version: "pgBackRest 2.59.3"}, restorer, h.ownership, dataRoot,
		func(_ context.Context, _ *v1.Project, message string) error {
			h.reported = append(h.reported, message)
			return nil
		}, zap.NewNop())
	h.restores.now = func() time.Time { return h.clock }
	h.restores.give = func(_ context.Context, _ string, owner pgbackrest.Owner, dir string) error {
		h.gave = append(h.gave, fmt.Sprintf("%d %s", owner.UID, filepath.Base(dir)))
		return nil
	}
	return h
}

// runOnce begins a restore and waits for it.
func (h *restoreHarness) runOnce(t *testing.T, p *v1.Project) *restore.PlacementBlockedError {
	t.Helper()
	released := make(chan struct{})
	blocked, took := h.restores.begin(context.Background(), p, func() { close(released) })
	require.True(t, took, "expected the restore to start, got %v", blocked)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore did not finish")
	}
	return blocked
}

func (h *restoreHarness) live(t *testing.T) string {
	t.Helper()
	return filepath.Join(h.dataRoot, "volumes", h.project.Namespace, h.project.Name, "pgdata", "data")
}

func TestRestoreUsesTheNewestAcceptedBackupAndInstallsIt(t *testing.T) {
	h := newRestoreHarness(t)

	blocked := h.runOnce(t, h.project)

	assert.Equal(t, restore.BlockRestoreInProgress, blocked.Reason)
	require.Len(t, h.runtime.specs, 1)
	spec := h.runtime.specs[0]
	assert.Equal(t, "new-label", spec.Cmd[len(spec.Cmd)-1])
	assert.Equal(t, docker.NetworkName(h.project.Name), spec.Network, "it must reach the repo host over the Project's network")
	assert.Equal(t, "tini", spec.Cmd[0], "an async archiver orphaned under the script must not reach a postmaster as PID 1")

	assert.FileExists(t, filepath.Join(h.live(t), "PG_VERSION"))
	marker, err := restore.ReadMarker(h.dataRoot, h.project.Namespace, h.project.Name)
	require.NoError(t, err)
	assert.True(t, marker.Matches(restore.ProvenanceFor(h.project, gateNode, "")))
	assert.Equal(t, "new", marker.InitializedFromBackupID)

	staging, err := restore.StagingPresent(h.dataRoot, h.project.Namespace, h.project.Name)
	require.NoError(t, err)
	assert.False(t, staging)
	require.Len(t, h.reported, 1)
	assert.Contains(t, h.reported[0], "last transaction at 2026-11-01 11:59:00 UTC")
}

// An Agent that is not root can move a directory only while it owns it, and the
// database must own it again before it starts.
func TestRestoreHandsTheDirectoryToTheAgentOnlyForTheSwap(t *testing.T) {
	h := newRestoreHarness(t)

	h.runOnce(t, h.project)

	db, agent := pgbackrest.ImageOwner.UID, os.Getuid()
	assert.Equal(t, []string{fmt.Sprintf("%d pgdata", db), fmt.Sprintf("%d pgdata", agent), fmt.Sprintf("%d data", db)}, h.gave)
}

func TestRestoreScriptReplaysToTheEndOfTheArchive(t *testing.T) {
	assert.NotContains(t, pgRestoreScript, "--type=", "the default type replays all archived WAL; immediate would stop at the backup")
	assert.NotContains(t, pgRestoreScript, "archive_mode=off", "the timeline history file must reach the repository")
}

func TestRestoreWithoutAnAcceptedBackupIsRefused(t *testing.T) {
	h := newRestoreHarness(t)
	h.project.Status.Backup = nil

	blocked, took := h.restores.begin(context.Background(), h.project, func() { t.Error("no job holds the claim") })

	assert.False(t, took)
	assert.Equal(t, restore.BlockNoRestoreSource, blocked.Reason)
	assert.Empty(t, h.runtime.specs)
}

func TestFailedRestoreLeavesLiveDataAndRetriesTheSameBackupWithBackoff(t *testing.T) {
	h := newRestoreHarness(t)
	require.NoError(t, os.MkdirAll(h.live(t), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(h.live(t), "existing"), []byte("x"), 0o600))
	good := h.runtime.run
	h.runtime.run = func(context.Context, docker.OneShotSpec) (docker.ExecResult, error) {
		return docker.ExecResult{ExitCode: 1, Stderr: "archive-get failed"}, nil
	}

	h.runOnce(t, h.project)

	assert.FileExists(t, filepath.Join(h.live(t), "existing"))
	assert.Empty(t, h.reported)

	blocked, took := h.restores.begin(context.Background(), h.project, func() {})
	assert.False(t, took, "the next poll must wait for the backoff")
	assert.Equal(t, restore.BlockRestoreFailed, blocked.Reason)
	assert.Contains(t, blocked.Detail, "archive-get failed")
	assert.Contains(t, blocked.Detail, "new")

	h.clock = h.clock.Add(pgRestoreMinBackoff + time.Second)
	h.runtime.run = good
	h.runOnce(t, h.project)

	require.Len(t, h.runtime.specs, 2)
	assert.Equal(t, h.runtime.specs[0].Cmd, h.runtime.specs[1].Cmd, "the same backup, never an older one")
	assert.FileExists(t, filepath.Join(h.live(t), "PG_VERSION"))
}

func TestRestoreBackoffIsBounded(t *testing.T) {
	assert.Equal(t, time.Minute, pgRestoreBackoff(1))
	assert.Equal(t, 2*time.Minute, pgRestoreBackoff(2))
	assert.Equal(t, 16*time.Minute, pgRestoreBackoff(5))
	assert.Equal(t, 30*time.Minute, pgRestoreBackoff(6))
	assert.Equal(t, 30*time.Minute, pgRestoreBackoff(60))
}

func TestRestoreStopsWhenTheAssignmentIsGone(t *testing.T) {
	h := newRestoreHarness(t)
	h.restores.ownershipCheck = time.Millisecond
	h.runtime.run = func(ctx context.Context, _ docker.OneShotSpec) (docker.ExecResult, error) {
		h.ownership.set(backup.OwnershipReassigned)
		<-ctx.Done()
		return docker.ExecResult{}, ctx.Err()
	}

	h.runOnce(t, h.project)

	assert.NoDirExists(t, h.live(t))
	assert.Empty(t, h.reported)
}

func TestRestoreIsNotInstalledForAnAssignmentThatEndedAsItFinished(t *testing.T) {
	h := newRestoreHarness(t)
	good := h.runtime.run
	h.runtime.run = func(ctx context.Context, spec docker.OneShotSpec) (docker.ExecResult, error) {
		h.ownership.set(backup.OwnershipLost)
		return good(ctx, spec)
	}

	h.runOnce(t, h.project)

	assert.NoDirExists(t, h.live(t))
}

func TestRestoreOutlivesAServerItCannotReach(t *testing.T) {
	h := newRestoreHarness(t)
	h.ownership.set(backup.OwnershipUnknown)

	h.runOnce(t, h.project)

	assert.FileExists(t, filepath.Join(h.live(t), "PG_VERSION"))
}

func TestRestorePreparesTheRepoHostFirst(t *testing.T) {
	h := newRestoreHarness(t)
	repo := &fakeRepo{authErr: errors.New("repo host down"), version: "pgBackRest 2.59.3"}
	h.restores.repo = repo

	h.runOnce(t, h.project)

	assert.Empty(t, h.runtime.specs, "nothing can reach the stanza until the repo host accepts this Project")
	blocked, _ := h.restores.begin(context.Background(), h.project, func() {})
	assert.Equal(t, restore.BlockRestoreFailed, blocked.Reason)
	assert.Contains(t, blocked.Detail, "repo host down")
}
