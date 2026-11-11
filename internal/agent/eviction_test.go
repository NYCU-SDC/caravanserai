package agent

import (
	"context"
	"errors"
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

var moveStart = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// projectBeingMoved is a Project with Managed data the server has asked this
// node to back up for a planned move.
func projectBeingMoved() *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: "uid-1"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "db", Image: "postgres:17"}},
			Volumes:  []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}},
		},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhaseRunning, NodeRef: "test-node", AssignmentGeneration: 3,
			Eviction: &v1.ProjectEviction{
				Reason: "Drain", FromNode: "test-node", State: v1.EvictionStateBackingUp,
				RequestedAt: moveStart, StateSince: moveStart,
			},
		},
	}
}

type fakeEvictionRunner struct {
	backupID    string
	leftStopped bool
	err         error
	block       chan struct{}

	mu        sync.Mutex
	calls     int
	confirmed *bool
}

func (f *fakeEvictionRunner) RunForEviction(ctx context.Context, _ *v1.Project, stillWanted func(context.Context) bool) (string, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return "", false, f.err
	}
	wanted := stillWanted(ctx)
	f.mu.Lock()
	f.confirmed = &wanted
	f.mu.Unlock()
	return f.backupID, f.leftStopped && wanted, nil
}

type conditionReport struct {
	Type    v1.ConditionType
	Status  v1.ConditionStatus
	Reason  string
	Message string
	Fence   AssignmentFence
}

type fakeEvictionClient struct {
	current    *v1.Project
	getErr     error
	stoppedErr error

	mu      sync.Mutex
	reports []conditionReport
	// stopped records workload-stopped evidence, in order with reports.
	stopped []AssignmentFence
	order   []string
}

func (f *fakeEvictionClient) ReportWorkloadStoppedForMove(_ context.Context, _ string, fence AssignmentFence, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stoppedErr != nil {
		return f.stoppedErr
	}
	f.stopped = append(f.stopped, fence)
	f.order = append(f.order, "workload-stopped")
	return nil
}

// fakeContainers records stops and starts.
type fakeContainers struct {
	stopErr error
	listErr error
	listed  []docker.ProjectContainer // what Docker lists once stopped

	mu    sync.Mutex
	calls []string
}

func (f *fakeContainers) StopProject(context.Context, *v1.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	return f.stopErr
}

func (f *fakeContainers) StartProject(context.Context, *v1.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	return nil
}

// StopAssignment counts as a stop, as StopProject does.
func (f *fakeContainers) StopAssignment(context.Context, *v1.Project) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	return f.stopErr
}

func (f *fakeContainers) ProjectContainers(context.Context, *v1.Project) ([]docker.ProjectContainer, error) {
	return f.listed, f.listErr
}

func (f *fakeEvictionClient) GetProject(context.Context, string) (*v1.Project, error) {
	return f.current, f.getErr
}

func (f *fakeEvictionClient) PatchProjectCondition(_ context.Context, _ string, fence AssignmentFence, t v1.ConditionType, s v1.ConditionStatus, reason, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, conditionReport{t, s, reason, message, fence})
	f.order = append(f.order, "result:"+reason)
	return nil
}

func runEvictionBackup(t *testing.T, runner evictionRunner, client *fakeEvictionClient, p *v1.Project) {
	t.Helper()
	b := newEvictionBackups(context.Background(), runner, &fakeContainers{}, nil, client, nil, zap.NewNop())
	b.handle(p)
	b.wait()
}

func TestEvictionBackups_Results(t *testing.T) {
	tests := []struct {
		name       string
		runner     *fakeEvictionRunner
		nilRunner  bool
		current    *v1.Project
		getErr     error
		wantStatus v1.ConditionStatus
		wantReason string
		wantNoCall bool
	}{
		{
			name:       "committed and still wanted is reported with the backup ID",
			runner:     &fakeEvictionRunner{backupID: "20261010T120000Z-abc", leftStopped: true},
			current:    projectBeingMoved(),
			wantStatus: v1.ConditionTrue, wantReason: evictionBackupSucceeded,
		},
		{
			name:       "a failed backup is reported",
			runner:     &fakeEvictionRunner{err: errors.New("disk full")},
			wantStatus: v1.ConditionFalse, wantReason: evictionBackupFailed,
		},
		{
			name:       "a move called off meanwhile is not reported as a success",
			runner:     &fakeEvictionRunner{backupID: "b", leftStopped: true},
			current:    func() *v1.Project { p := projectBeingMoved(); p.Status.Eviction = nil; return p }(),
			wantStatus: v1.ConditionFalse, wantReason: evictionBackupNotConfirmed,
		},
		{
			name:       "a move that cannot be confirmed is not reported as a success",
			runner:     &fakeEvictionRunner{backupID: "b", leftStopped: true},
			getErr:     errors.New("server unreachable"),
			wantStatus: v1.ConditionFalse, wantReason: evictionBackupNotConfirmed,
		},
		{
			name:       "no object store fails the move",
			nilRunner:  true,
			wantStatus: v1.ConditionFalse, wantReason: evictionBackupNoObjectStore,
		},
		{
			name:       "a busy project reports nothing and is retried next poll",
			runner:     &fakeEvictionRunner{err: backup.ErrProjectBusy},
			wantNoCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeEvictionClient{current: tt.current, getErr: tt.getErr}
			var runner evictionRunner
			if !tt.nilRunner {
				runner = tt.runner
			}
			runEvictionBackup(t, runner, client, projectBeingMoved())

			if tt.wantNoCall {
				assert.Empty(t, client.reports)
				return
			}
			require.Len(t, client.reports, 1)
			r := client.reports[0]
			assert.Equal(t, v1.ConditionTypeEvictionBackup, r.Type)
			assert.Equal(t, tt.wantStatus, r.Status)
			assert.Equal(t, tt.wantReason, r.Reason)
			assert.Equal(t, AssignmentFence{UID: "uid-1", NodeRef: "test-node", Generation: 3}, r.Fence,
				"the report is fenced to this assignment")
			if tt.wantReason == evictionBackupSucceeded {
				assert.Equal(t, "20261010T120000Z-abc", r.Message)
			}
		})
	}
}

func TestEvictionBackups_StillWanted(t *testing.T) {
	tests := []struct {
		name    string
		current func(p *v1.Project)
		want    bool
	}{
		{name: "same move", current: func(*v1.Project) {}, want: true},
		{name: "a different move", current: func(p *v1.Project) { p.Status.Eviction.StateSince = moveStart.Add(time.Minute) }, want: false},
		{name: "already moving", current: func(p *v1.Project) { p.Status.Eviction.State = v1.EvictionStateMoving }, want: false},
		{name: "move dropped", current: func(p *v1.Project) { p.Status.Eviction = nil }, want: false},
		{name: "assigned elsewhere", current: func(p *v1.Project) { p.Status.NodeRef = "other" }, want: false},
		{name: "no longer running", current: func(p *v1.Project) { p.Status.Phase = v1.ProjectPhasePending }, want: false},
		{name: "a different project of the same name", current: func(p *v1.Project) { p.ObjectMeta.UID = "uid-2" }, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := projectBeingMoved()
			tt.current(cur)
			b := newEvictionBackups(context.Background(), nil, &fakeContainers{}, nil, &fakeEvictionClient{current: cur}, nil, zap.NewNop())
			assert.Equal(t, tt.want, b.stillWanted(projectBeingMoved())(context.Background()))
		})
	}
}

func TestEvictionBackups_RunsOnce(t *testing.T) {
	t.Run("not started twice while running", func(t *testing.T) {
		runner := &fakeEvictionRunner{backupID: "b", leftStopped: true, block: make(chan struct{})}
		client := &fakeEvictionClient{current: projectBeingMoved()}
		b := newEvictionBackups(context.Background(), runner, &fakeContainers{}, nil, client, nil, zap.NewNop())

		b.handle(projectBeingMoved())
		b.handle(projectBeingMoved())
		close(runner.block)
		b.wait()
		assert.Equal(t, 1, runner.calls)
	})

	t.Run("not started again once this move's result is reported", func(t *testing.T) {
		p := projectBeingMoved()
		p.Status.Conditions = []v1.Condition{{Type: v1.ConditionTypeEvictionBackup, Status: v1.ConditionTrue, LastTransitionTime: moveStart.Add(time.Second)}}
		runner := &fakeEvictionRunner{}
		runEvictionBackup(t, runner, &fakeEvictionClient{}, p)
		assert.Equal(t, 0, runner.calls)
	})

	t.Run("a result from an earlier move does not count", func(t *testing.T) {
		p := projectBeingMoved()
		p.Status.Conditions = []v1.Condition{{Type: v1.ConditionTypeEvictionBackup, Status: v1.ConditionTrue, LastTransitionTime: moveStart.Add(-time.Hour)}}
		runner := &fakeEvictionRunner{backupID: "b", leftStopped: true}
		runEvictionBackup(t, runner, &fakeEvictionClient{current: projectBeingMoved()}, p)
		assert.Equal(t, 1, runner.calls)
	})

	t.Run("only a running project in BackingUp is acted on", func(t *testing.T) {
		for _, mutate := range []func(*v1.Project){
			func(p *v1.Project) { p.Status.Eviction = nil },
			func(p *v1.Project) { p.Status.Eviction.State = v1.EvictionStateQueued },
			func(p *v1.Project) { p.Status.Phase = v1.ProjectPhaseTerminating },
		} {
			p := projectBeingMoved()
			mutate(p)
			runner := &fakeEvictionRunner{}
			runEvictionBackup(t, runner, &fakeEvictionClient{}, p)
			assert.Equal(t, 0, runner.calls)
		}
	})
}

func TestReconcileProjectsLeavesProjectUnderEvictionAlone(t *testing.T) {
	// The move left the containers stopped. Without the skip the health check
	// would report the Project Failed and recovery would start it again.
	moved := runningProject("blog")
	moved.Spec.Volumes = projectBeingMoved().Spec.Volumes
	moved.Status.Eviction = projectBeingMoved().Status.Eviction
	client, updates := newReconcileServer(t, []v1.Project{moved})

	var inspected bool
	rt := &mockRuntime{
		inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			inspected = true
			return nil, nil
		},
	}
	runner := &fakeEvictionRunner{err: backup.ErrProjectBusy}
	evictions := newEvictionBackups(context.Background(), runner, &fakeContainers{}, nil, &fakeEvictionClient{}, nil, zap.NewNop())

	reconcileProjects(context.Background(), client, rt, nil, nil, evictions, nil, nil, nil, nil, zap.NewNop())
	evictions.wait()

	assert.False(t, inspected, "a project stopped for a move is not health-checked")
	assert.Empty(t, *updates, "and no status is written for it")
	assert.Equal(t, 1, runner.calls, "the move's backup is started instead")
}

func TestEvictionBackups_ConfirmsStopBeforeReportingSuccess(t *testing.T) {
	client := &fakeEvictionClient{current: projectBeingMoved()}
	runEvictionBackup(t, &fakeEvictionRunner{backupID: "b1", leftStopped: true}, client, projectBeingMoved())

	assert.Equal(t, []string{"workload-stopped", "result:" + evictionBackupSucceeded}, client.order,
		"the release gate needs the stop evidence; it is recorded before the result")
	assert.Equal(t, []AssignmentFence{{UID: "uid-1", NodeRef: "test-node", Generation: 3}}, client.stopped)
}

func TestEvictionBackups_NoResultWhenStopEvidenceIsRefused(t *testing.T) {
	client := &fakeEvictionClient{current: projectBeingMoved(), stoppedErr: errors.New("server unreachable")}
	runEvictionBackup(t, &fakeEvictionRunner{backupID: "b1", leftStopped: true}, client, projectBeingMoved())
	assert.Empty(t, client.reports, "without the evidence the move cannot go on; the next poll tries again")
}

func TestEvictionBackups_ProjectWithoutData(t *testing.T) {
	noData := func() *v1.Project {
		p := projectBeingMoved()
		p.Spec.Volumes = nil
		return p
	}

	t.Run("is stopped, confirmed and reported, with no backup", func(t *testing.T) {
		containers := &fakeContainers{}
		runner := &fakeEvictionRunner{}
		client := &fakeEvictionClient{current: noData()}
		b := newEvictionBackups(context.Background(), runner, containers, nil, client, nil, zap.NewNop())
		b.handle(noData())
		b.wait()

		assert.Equal(t, []string{"stop"}, containers.calls, "left stopped for the move")
		assert.Equal(t, 0, runner.calls, "nothing to back up")
		assert.Equal(t, []string{"workload-stopped", "result:" + evictionStoppedNoData}, client.order)
	})

	t.Run("works on an agent with no object store", func(t *testing.T) {
		client := &fakeEvictionClient{current: noData()}
		b := newEvictionBackups(context.Background(), nil, &fakeContainers{}, nil, client, nil, zap.NewNop())
		b.handle(noData())
		b.wait()
		require.Len(t, client.reports, 1)
		assert.Equal(t, v1.ConditionTrue, client.reports[0].Status)
	})

	t.Run("is started again when the move cannot be confirmed", func(t *testing.T) {
		containers := &fakeContainers{}
		client := &fakeEvictionClient{getErr: errors.New("server unreachable")}
		b := newEvictionBackups(context.Background(), nil, containers, nil, client, nil, zap.NewNop())
		b.handle(noData())
		b.wait()

		assert.Equal(t, []string{"stop", "start"}, containers.calls)
		assert.Empty(t, client.stopped, "no stop evidence for a project that is running again")
		require.Len(t, client.reports, 1)
		assert.Equal(t, evictionBackupNotConfirmed, client.reports[0].Reason)
	})

	t.Run("is started again when it cannot be stopped", func(t *testing.T) {
		containers := &fakeContainers{stopErr: errors.New("docker down")}
		client := &fakeEvictionClient{current: noData()}
		b := newEvictionBackups(context.Background(), nil, containers, nil, client, nil, zap.NewNop())
		b.handle(noData())
		b.wait()

		assert.Equal(t, []string{"stop", "start"}, containers.calls)
		assert.Empty(t, client.stopped)
		require.Len(t, client.reports, 1)
		assert.Equal(t, evictionStopFailed, client.reports[0].Reason)
	})
}

func TestEvictionBackups_PostgresDifferentialIsNotMoved(t *testing.T) {
	p := projectBeingMoved()
	p.Spec.Backup = &v1.ProjectBackupConfig{Interval: "1h", Items: []v1.BackupItem{{Volume: "data", Strategy: v1.BackupStrategyPostgresDifferential}}}
	containers := &fakeContainers{}
	runner := &fakeEvictionRunner{}
	client := &fakeEvictionClient{current: p}
	b := newEvictionBackups(context.Background(), runner, containers, nil, client, nil, zap.NewNop())
	b.handle(p)
	b.wait()

	assert.Empty(t, containers.calls, "left running")
	assert.Equal(t, 0, runner.calls)
	assert.Empty(t, client.stopped)
	require.Len(t, client.reports, 1)
	assert.Equal(t, evictionUnsupportedStrategy, client.reports[0].Reason)
}

// ── Once the stop is reported (CARA-92 latch, review A2) ─────────────────────

// The server holds this move's stop evidence; its result report was lost. The
// next poll must not stop, back up or — on a failing second backup — start
// the Project again: the server moves it on the evidence alone.
func TestEvictionBackups_ReportedStopIsNotRedone(t *testing.T) {
	p := projectBeingMoved()
	latch(p, p.Status.AssignmentGeneration)
	containers := &fakeContainers{}
	runner := &fakeEvictionRunner{err: errors.New("second backup fails")}
	client := &fakeEvictionClient{current: p}
	b := newEvictionBackups(context.Background(), runner, containers, nil, client, NewStopLatches(t.TempDir()), zap.NewNop())

	b.handle(p)
	b.wait()

	assert.Zero(t, runner.calls, "no second backup")
	assert.Empty(t, containers.calls, "the containers are not touched")
	assert.Empty(t, client.reports)
}

// Recorded here, not seen accepted by the server: only the evidence is sent
// again.
func TestEvictionBackups_RecordedStopIsOnlySentAgain(t *testing.T) {
	p := projectBeingMoved()
	latches := NewStopLatches(t.TempDir())
	require.NoError(t, latches.Record(p, time.Now()))
	containers := &fakeContainers{}
	runner := &fakeEvictionRunner{backupID: "b-2", leftStopped: true}
	client := &fakeEvictionClient{current: p}
	b := newEvictionBackups(context.Background(), runner, containers, nil, client, latches, zap.NewNop())

	b.handle(p)
	b.wait()

	assert.Zero(t, runner.calls)
	assert.Empty(t, containers.calls)
	assert.Equal(t, []string{"workload-stopped"}, client.order)
}

func TestEvictionBackups_ConfirmingTheStop(t *testing.T) {
	committed := func() *fakeEvictionRunner { return &fakeEvictionRunner{backupID: "b-1", leftStopped: true} }

	t.Run("the stop is recorded before the evidence is sent", func(t *testing.T) {
		p := projectBeingMoved()
		latches := NewStopLatches(t.TempDir())
		client := &fakeEvictionClient{current: p}
		b := newEvictionBackups(context.Background(), committed(), &fakeContainers{}, nil, client, latches, zap.NewNop())
		b.handle(p)
		b.wait()

		held, err := latches.Holds(p)
		require.NoError(t, err)
		assert.True(t, held)
		assert.Equal(t, []string{"workload-stopped", "result:" + evictionBackupSucceeded}, client.order)
	})

	t.Run("a container the spec no longer names keeps the move from going on", func(t *testing.T) {
		p := projectBeingMoved()
		containers := &fakeContainers{listed: []docker.ProjectContainer{
			{Name: "blog-db", Service: "db", State: "exited"},
			{Name: "blog-worker", Service: "worker", State: "running"},
		}}
		client := &fakeEvictionClient{current: p}
		b := newEvictionBackups(context.Background(), committed(), containers, nil, client, NewStopLatches(t.TempDir()), zap.NewNop())
		b.handle(p)
		b.wait()

		assert.Empty(t, client.stopped, "no evidence while a copy of the workload runs")
		assert.Equal(t, []string{"stop", "start"}, containers.calls, "stopped by label, then started again")
		require.Len(t, client.reports, 1)
		assert.Equal(t, evictionStopFailed, client.reports[0].Reason)
	})

	t.Run("a refused report drops the record and leaves the restart to the poll loop", func(t *testing.T) {
		p := projectBeingMoved()
		latches := NewStopLatches(t.TempDir())
		containers := &fakeContainers{}
		client := &fakeEvictionClient{current: p, stoppedErr: ErrStaleAssignment}
		b := newEvictionBackups(context.Background(), committed(), containers, nil, client, latches, zap.NewNop())
		b.handle(p)
		b.wait()

		held, err := latches.Holds(p)
		require.NoError(t, err)
		assert.False(t, held, "the server holds no evidence: nothing to guard")
		assert.NotContains(t, containers.calls, "start", "whether it is still this node's is the poll loop's to judge")
		assert.Empty(t, client.reports, "no result for a move the server called off")
	})

	t.Run("an unanswered report keeps the record", func(t *testing.T) {
		p := projectBeingMoved()
		latches := NewStopLatches(t.TempDir())
		client := &fakeEvictionClient{current: p, stoppedErr: errors.New("timeout")}
		b := newEvictionBackups(context.Background(), committed(), &fakeContainers{}, nil, client, latches, zap.NewNop())
		b.handle(p)
		b.wait()

		held, err := latches.Holds(p)
		require.NoError(t, err)
		assert.True(t, held)
	})
}

// The extra guard the review asked for: a restart never undoes a stop this
// node recorded, whichever path asks for it.
func TestEvictionBackups_RestartRespectsARecordedStop(t *testing.T) {
	p := projectBeingMoved()
	latches := NewStopLatches(t.TempDir())
	require.NoError(t, latches.Record(p, time.Now()))
	containers := &fakeContainers{}
	b := newEvictionBackups(context.Background(), nil, containers, nil, &fakeEvictionClient{current: p}, latches, zap.NewNop())

	b.restart(zap.NewNop(), p)

	assert.Empty(t, containers.calls)
}
