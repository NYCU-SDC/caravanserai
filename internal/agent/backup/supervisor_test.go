package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestSupervisor builds a Supervisor whose Runner archives real (tiny)
// volumes into temp dirs but writes to an in-memory store, so scheduling
// behaviour is observable end to end.
func newTestSupervisor(t *testing.T) (*Supervisor, *fakeStore, *fakeContainers) {
	t.Helper()

	dataRoot := t.TempDir()
	containers := &fakeContainers{}
	store := newFakeStore()

	runner := NewRunner(
		NewCoordinator(), containers, store,
		fakeOwnership{result: OwnershipRetained}, &fakeConditions{}, &fakeRoutes{},
		Config{DataRoot: dataRoot, NodeName: "node-a", CaraVersion: "test"},
		zap.NewNop(),
	)

	return NewSupervisor(runner, zap.NewNop()), store, containers
}

// supervisedProject builds a Running Project with one Managed volume backed
// by a real directory under the runner's data root.
func supervisedProject(t *testing.T, dataRoot, name, interval string) *v1.Project {
	t.Helper()
	dir := filepath.Join(dataRoot, "volumes", "default", name, "data-vol", "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600))

	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: "node-a"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Volumes:  []v1.VolumeDef{{Name: "data-vol", Type: v1.VolumeTypeManaged}},
			Backup:   &v1.ProjectBackupConfig{Interval: interval},
		},
	}
}

func TestSupervisorStartsAndStopsWithProjects(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "1h")
	s.Sync(ctx, []*v1.Project{p})
	assert.Equal(t, 1, s.Count())

	// The Project leaves this node: its goroutine must be cancelled.
	s.Sync(ctx, nil)
	assert.Equal(t, 0, s.Count())
}

func TestSupervisorDoesNotDuplicateAcrossPolls(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()
	p := supervisedProject(t, dataRoot, "blog", "1h")

	// The poll loop calls Sync every tick; repeated calls must not spawn a
	// second goroutine for the same Project.
	for i := 0; i < 5; i++ {
		s.Sync(ctx, []*v1.Project{p})
	}

	assert.Equal(t, 1, s.Count())
}

func TestSupervisorIgnoresProjectsWithoutBackupPolicy(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "1h")
	p.Spec.Backup = nil

	s.Sync(ctx, []*v1.Project{p})
	assert.Equal(t, 0, s.Count(), "a Managed volume without a backup policy is persisted, not backed up")
}

func TestSupervisorIgnoresProjectsWithoutManagedVolumes(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "1h")
	p.Spec.Volumes = []v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}

	s.Sync(ctx, []*v1.Project{p})
	assert.Equal(t, 0, s.Count())
}

func TestSupervisorIgnoresNonRunningProjects(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	for _, phase := range []v1.ProjectPhase{
		v1.ProjectPhasePending, v1.ProjectPhaseScheduled,
		v1.ProjectPhaseTerminating, v1.ProjectPhaseFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			p := supervisedProject(t, dataRoot, "blog", "1h")
			p.Status.Phase = phase

			s.Sync(ctx, []*v1.Project{p})
			assert.Equal(t, 0, s.Count())
		})
	}
}

func TestSupervisorIgnoresUnparsableInterval(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "not-a-duration")

	s.Sync(ctx, []*v1.Project{p})
	assert.Equal(t, 0, s.Count(), "an unusable interval must be skipped, not crash the agent")
}

func TestSupervisorRestartsWhenIntervalChanges(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "1h")
	s.Sync(ctx, []*v1.Project{p})
	require.Equal(t, 1, s.Count())

	key := ResourceKey{Namespace: "default", Name: "blog"}
	s.mu.Lock()
	first := s.entries[key]
	s.mu.Unlock()

	// A changed cadence needs a new timer, so the entry must be replaced.
	updated := supervisedProject(t, dataRoot, "blog", "30m")
	s.Sync(ctx, []*v1.Project{updated})

	s.mu.Lock()
	second := s.entries[key]
	s.mu.Unlock()

	assert.Equal(t, 1, s.Count())
	assert.NotSame(t, first, second, "a changed interval must rebuild the supervisor")
	assert.Equal(t, 30*time.Minute, second.interval)
}

func TestSupervisorRefreshesSpecWithoutRestarting(t *testing.T) {
	s, _, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "1h")
	s.Sync(ctx, []*v1.Project{p})

	key := ResourceKey{Namespace: "default", Name: "blog"}
	s.mu.Lock()
	first := s.entries[key]
	s.mu.Unlock()

	// Same interval, newer spec: the goroutine survives but the next run must
	// see the updated Project.
	updated := supervisedProject(t, dataRoot, "blog", "1h")
	updated.ResourceVersion = 99
	s.Sync(ctx, []*v1.Project{updated})

	s.mu.Lock()
	second := s.entries[key]
	s.mu.Unlock()

	assert.Same(t, first, second, "an unchanged interval must not restart the goroutine")
	assert.Equal(t, int64(99), second.current().ResourceVersion)
}

func TestSupervisorRunsBackupOnSchedule(t *testing.T) {
	s, store, containers := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := supervisedProject(t, dataRoot, "blog", "30ms")
	s.Sync(ctx, []*v1.Project{p})

	latestKey, err := LatestKey("default", "blog")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return store.has(latestKey)
	}, 2*time.Second, 10*time.Millisecond, "a scheduled backup should have completed")

	assert.Contains(t, containers.history(), "stop")
	assert.Contains(t, containers.history(), "start")
}

func TestSupervisorStopHaltsScheduledBackups(t *testing.T) {
	s, store, _ := newTestSupervisor(t)
	dataRoot := s.runner.cfg.DataRoot
	ctx := context.Background()

	p := supervisedProject(t, dataRoot, "blog", "30ms")
	s.Sync(ctx, []*v1.Project{p})

	latestKey, err := LatestKey("default", "blog")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.has(latestKey) }, 2*time.Second, 10*time.Millisecond)

	s.Stop()
	assert.Equal(t, 0, s.Count())

	// No further backups after Stop.
	before := len(store.keys())
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, before, len(store.keys()), "Stop must halt scheduled backups")
}

func TestShouldSupervise(t *testing.T) {
	base := func() *v1.Project {
		return &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default"},
			Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
			Spec: v1.ProjectSpec{
				Volumes: []v1.VolumeDef{{Name: "db", Type: v1.VolumeTypeManaged}},
				Backup:  &v1.ProjectBackupConfig{Interval: "1h"},
			},
		}
	}

	assert.True(t, shouldSupervise(base()))

	noBackup := base()
	noBackup.Spec.Backup = nil
	assert.False(t, shouldSupervise(noBackup))

	notRunning := base()
	notRunning.Status.Phase = v1.ProjectPhaseFailed
	assert.False(t, shouldSupervise(notRunning))

	ephemeralOnly := base()
	ephemeralOnly.Spec.Volumes = []v1.VolumeDef{{Name: "c", Type: v1.VolumeTypeEphemeral}}
	assert.False(t, shouldSupervise(ephemeralOnly))
}
