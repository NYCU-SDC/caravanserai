package pgbackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var t0 = time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)

func set(label, typ string, start time.Time, timeline string) backupSet {
	var b backupSet
	b.Label, b.Type = label, typ
	b.Timestamp.Start, b.Timestamp.Stop = start.Unix(), start.Add(time.Minute).Unix()
	b.Archive.Start = timeline + "0000000000000003"
	return b
}

func TestChooseType(t *testing.T) {
	full := set("20261001-030000F", TypeFull, t0, "00000001")
	diff := set("20261001-030000F_20261002-030000D", TypeDiff, t0.Add(24*time.Hour), "00000001")
	cases := []struct {
		name     string
		sets     []backupSet
		timeline string
		known    bool
		now      time.Time
		want     string
	}{
		{"no backup", nil, "00000001", true, t0, TypeFull},
		{"only a differential", []backupSet{diff}, "00000001", true, t0, TypeFull},
		{"recent full", []backupSet{full, diff}, "00000001", true, t0.Add(2 * 24 * time.Hour), TypeDiff},
		{"full just inside the interval", []backupSet{full}, "00000001", true, t0.Add(FullInterval - time.Second), TypeDiff},
		{"full at the interval", []backupSet{full, diff}, "00000001", true, t0.Add(FullInterval), TypeFull},
		{"new timeline", []backupSet{full}, "00000002", true, t0.Add(time.Hour), TypeFull},
		{"continuity unknown", []backupSet{full}, "00000001", false, t0.Add(time.Hour), TypeFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseType(tc.sets, tc.timeline, tc.known, tc.now, FullInterval)
			assert.Equal(t, tc.want, got.Type, got.Reason)
			assert.NotEmpty(t, got.Reason)
		})
	}
}

func TestParseInfoRefusesAStanzaItCannotRead(t *testing.T) {
	_, err := parseInfo([]byte(`[{"name":"s","backup":[],"status":{"code":1,"message":"missing stanza path"}}]`), "s")
	assert.Error(t, err)
	_, err = parseInfo([]byte(`[{"name":"other","backup":[],"status":{"code":0}}]`), "s")
	assert.Error(t, err)
	sets, err := parseInfo([]byte(`[{"name":"s","backup":[],"status":{"code":2,"message":"no valid backups"}}]`), "s")
	require.NoError(t, err)
	assert.Empty(t, sets)
}

// fakeDB simulates the database container: pgbackrest info, backup and psql.
type fakeDB struct {
	mu       sync.Mutex
	sets     []backupSet
	timeline string
	now      func() time.Time
	// backupErr fails the next backup; backupNoop succeeds without adding one.
	backupExit int
	backupNoop bool
	// block makes a backup wait until stopped.
	block   chan struct{}
	stopped bool
	backups []string
	states  []docker.ContainerState
}

func newFakeDB(now func() time.Time) *fakeDB {
	return &fakeDB{timeline: "00000001", now: now,
		states: []docker.ContainerState{{ServiceName: "db", ContainerID: "cid-1", Status: "running"}}}
}

func (f *fakeDB) InspectProject(context.Context, *v1.Project) ([]docker.ContainerState, error) {
	return f.states, nil
}

func (f *fakeDB) Exec(_ context.Context, _, _ string, cmd []string) (docker.ExecResult, error) {
	line := strings.Join(cmd, " ")
	switch {
	case cmd[0] == "sh" && cmd[len(cmd)-2] == "stop-backup":
		f.mu.Lock()
		f.stopped = true
		if f.block != nil {
			close(f.block)
			f.block = nil
		}
		f.mu.Unlock()
		return docker.ExecResult{}, nil
	case cmd[0] == "sh":
		sql := cmd[len(cmd)-1]
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(sql, "system_identifier"):
			return docker.ExecResult{Stdout: "7400000000000000001\n"}, nil
		case strings.Contains(sql, "pg_walfile_name"):
			return docker.ExecResult{Stdout: f.timeline + "\n"}, nil
		case strings.Contains(sql, "server_version_num"):
			return docker.ExecResult{Stdout: "160004\n"}, nil
		}
	case strings.HasSuffix(line, " info"):
		f.mu.Lock()
		defer f.mu.Unlock()
		out, _ := json.Marshal([]map[string]any{{"name": strings.TrimPrefix(cmd[1], "--stanza="), "backup": f.sets,
			"status": map[string]any{"code": 0}}})
		return docker.ExecResult{Stdout: string(out)}, nil
	case strings.HasSuffix(line, " backup"):
		f.mu.Lock()
		block := f.block
		f.backups = append(f.backups, cmd[2])
		f.mu.Unlock()
		if block != nil {
			<-block
			return docker.ExecResult{ExitCode: 143, Stderr: "terminated"}, nil
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.backupExit != 0 {
			return docker.ExecResult{ExitCode: f.backupExit, Stderr: "ERROR: [056]: unable to find primary cluster"}, nil
		}
		if !f.backupNoop {
			typ := strings.TrimPrefix(cmd[2], "--type=")
			label := fmt.Sprintf("%s%s", f.now().Format("20060102-150405"), map[string]string{TypeFull: "F", TypeDiff: "D"}[typ])
			f.sets = append(f.sets, set(label, typ, f.now(), f.timeline))
		}
		return docker.ExecResult{}, nil
	}
	return docker.ExecResult{ExitCode: 127, Stderr: "unexpected " + line}, nil
}

type fakeContinuity struct {
	known    bool
	version  uint64
	checkErr error
	cleared  []uint64
}

func (c *fakeContinuity) Check(context.Context, Database) (bool, uint64, error) {
	return c.known, c.version, c.checkErr
}

func (c *fakeContinuity) FullAccepted(_ context.Context, _ Database, v uint64) error {
	c.cleared = append(c.cleared, v)
	return nil
}

type fakeRecorder struct {
	err   error
	items [][]Item
}

func (r *fakeRecorder) Record(_ context.Context, _ *v1.Project, _ string, items []Item) error {
	r.items = append(r.items, items)
	return r.err
}

func pgProject() *v1.Project {
	p := &v1.Project{}
	p.Namespace, p.Name, p.UID = "default", "blog", "uid-1"
	p.Status.Phase = v1.ProjectPhaseRunning
	p.Spec.Volumes = []v1.VolumeDef{{Name: "pgdata", Type: v1.VolumeTypeManaged}}
	p.Spec.Services = []v1.ServiceDef{
		{Name: "db", Image: v1.PostgresDifferentialImage,
			VolumeMounts: []v1.VolumeMount{{Name: "pgdata", MountPath: v1.PostgresDifferentialDataPath}}},
		{Name: "web", Image: "nginx"},
	}
	p.Spec.Backup = &v1.ProjectBackupConfig{Interval: "24h", Items: []v1.BackupItem{
		{Volume: "pgdata", Strategy: v1.BackupStrategyPostgresDifferential},
	}}
	return p
}

type harness struct {
	runner *Runner
	db     *fakeDB
	cont   *fakeContinuity
	rec    *fakeRecorder
	clock  time.Time
}

func newHarness() *harness {
	h := &harness{clock: t0, cont: &fakeContinuity{known: true, version: 3}, rec: &fakeRecorder{}}
	now := func() time.Time { return h.clock }
	h.db = newFakeDB(now)
	h.runner = NewRunner(backup.NewCoordinator(), h.db, h.cont, h.rec, zap.NewNop())
	h.runner.now = now
	return h
}

// A full first, then differentials until the full interval passes. No
// service is stopped: the runner has no way to.
func TestRunnerTakesFullThenDifferential(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	require.NoError(t, h.runner.Run(ctx, pgProject()))
	h.clock = h.clock.Add(24 * time.Hour)
	require.NoError(t, h.runner.Run(ctx, pgProject()))
	h.clock = h.clock.Add(FullInterval)
	require.NoError(t, h.runner.Run(ctx, pgProject()))

	assert.Equal(t, []string{"--type=full", "--type=diff", "--type=full"}, h.db.backups)
	require.Len(t, h.rec.items, 3)
	first := h.rec.items[0][0]
	assert.Equal(t, Item{Volume: "pgdata", Stanza: "uid-1-pgdata", Label: "20261001-030000F", Type: TypeFull, PGMajor: 16,
		Started: t0, Stopped: t0.Add(time.Minute)}, first)
	assert.Equal(t, []uint64{3, 3}, h.cont.cleared, "each accepted full clears at the version it started at")
}

// A failed backup, and a success that info does not confirm, record nothing
// and clear nothing.
func TestRunnerRecordsOnlyConfirmedBackups(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeDB)
	}{
		{"backup fails", func(f *fakeDB) { f.backupExit = 56 }},
		{"no new label", func(f *fakeDB) { f.backupNoop = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			tc.setup(h.db)
			assert.Error(t, h.runner.Run(context.Background(), pgProject()))
			assert.Empty(t, h.rec.items)
			assert.Empty(t, h.cont.cleared)
		})
	}
}

// An earlier backup in info is not mistaken for the one just taken.
func TestRunnerDoesNotConfirmWithAnOlderLabel(t *testing.T) {
	h := newHarness()
	require.NoError(t, h.runner.Run(context.Background(), pgProject()))
	h.clock = h.clock.Add(FullInterval)
	h.db.backupNoop = true
	assert.Error(t, h.runner.Run(context.Background(), pgProject()))
	assert.Len(t, h.rec.items, 1)
}

// A full the server did not accept does not clear the latch.
func TestRunnerKeepsTheLatchWhenTheRecordIsRefused(t *testing.T) {
	h := newHarness()
	h.rec.err = errors.New("409 stale assignment")
	assert.Error(t, h.runner.Run(context.Background(), pgProject()))
	assert.Len(t, h.rec.items, 1)
	assert.Empty(t, h.cont.cleared)
}

// Continuity that is unknown, or cannot be checked, forces full.
func TestRunnerTakesFullWhenContinuityIsNotKnown(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	require.NoError(t, h.runner.Run(ctx, pgProject()))
	h.clock = h.clock.Add(time.Hour)

	h.cont.known = false
	require.NoError(t, h.runner.Run(ctx, pgProject()))
	h.clock = h.clock.Add(time.Hour)

	h.cont.known, h.cont.checkErr = true, errors.New("log unreadable")
	require.NoError(t, h.runner.Run(ctx, pgProject()))

	assert.Equal(t, []string{"--type=full", "--type=full", "--type=full"}, h.db.backups)
}

// After a promote, the database is on a new timeline: full.
func TestRunnerTakesFullOnANewTimeline(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	require.NoError(t, h.runner.Run(ctx, pgProject()))
	h.clock = h.clock.Add(time.Hour)
	h.db.timeline = "00000002"
	require.NoError(t, h.runner.Run(ctx, pgProject()))
	assert.Equal(t, []string{"--type=full", "--type=full"}, h.db.backups)
}

// Nothing runs in a container that is not this assignment's, or not running.
func TestRunnerRefusesAContainerItDoesNotOwn(t *testing.T) {
	for _, st := range []docker.ContainerState{
		{ServiceName: "db", ContainerID: "cid-1", Status: "running", NotOwned: errors.New("generation 6")},
		{ServiceName: "db", ContainerID: "cid-1", Status: "exited"},
	} {
		h := newHarness()
		h.db.states = []docker.ContainerState{st}
		err := h.runner.Run(context.Background(), pgProject())
		assert.ErrorIs(t, err, ErrContainerNotReady)
		assert.Empty(t, h.db.backups)
	}
}

// A run that finds the Project busy does nothing.
func TestRunnerWaitsForTheClaim(t *testing.T) {
	h := newHarness()
	release, ok := h.runner.coordinator.TryClaim(backup.ResourceKey{Namespace: "default", Name: "blog"}, backup.OpRecovery)
	require.True(t, ok)
	defer release()
	require.NoError(t, h.runner.Run(context.Background(), pgProject()))
	assert.Empty(t, h.db.backups)
}

// Cancelling a run stops the backup inside the container rather than leaving
// it to finish unobserved.
func TestRunnerStopsTheBackupWhenCancelled(t *testing.T) {
	h := newHarness()
	h.db.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.Run(ctx, pgProject()) }()

	require.Eventually(t, func() bool {
		h.db.mu.Lock()
		defer h.db.mu.Unlock()
		return len(h.db.backups) == 1
	}, time.Second, time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled run did not return")
	}
	assert.True(t, h.db.stopped)
	assert.Empty(t, h.rec.items)
}
