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

// projectBeingMoved is a Project the server has asked this node to back up
// for a planned move.
func projectBeingMoved() *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: "uid-1"},
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
	current *v1.Project
	getErr  error

	mu      sync.Mutex
	reports []conditionReport
}

func (f *fakeEvictionClient) GetProject(context.Context, string) (*v1.Project, error) {
	return f.current, f.getErr
}

func (f *fakeEvictionClient) PatchProjectCondition(_ context.Context, _ string, fence AssignmentFence, t v1.ConditionType, s v1.ConditionStatus, reason, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, conditionReport{t, s, reason, message, fence})
	return nil
}

func runEvictionBackup(t *testing.T, runner evictionRunner, client *fakeEvictionClient, p *v1.Project) {
	t.Helper()
	b := newEvictionBackups(context.Background(), runner, client, zap.NewNop())
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
			b := newEvictionBackups(context.Background(), nil, &fakeEvictionClient{current: cur}, zap.NewNop())
			assert.Equal(t, tt.want, b.stillWanted(projectBeingMoved())(context.Background()))
		})
	}
}

func TestEvictionBackups_RunsOnce(t *testing.T) {
	t.Run("not started twice while running", func(t *testing.T) {
		runner := &fakeEvictionRunner{backupID: "b", leftStopped: true, block: make(chan struct{})}
		client := &fakeEvictionClient{current: projectBeingMoved()}
		b := newEvictionBackups(context.Background(), runner, client, zap.NewNop())

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
	evictions := newEvictionBackups(context.Background(), runner, &fakeEvictionClient{}, zap.NewNop())

	reconcileProjects(context.Background(), client, rt, nil, nil, evictions, nil, nil, false, zap.NewNop())
	evictions.wait()

	assert.False(t, inspected, "a project stopped for a move is not health-checked")
	assert.Empty(t, *updates, "and no status is written for it")
	assert.Equal(t, 1, runner.calls, "the move's backup is started instead")
}
