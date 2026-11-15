package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func failedProject(reason string) *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: "uid-1"},
		Spec: v1.ProjectSpec{Services: []v1.ServiceDef{
			{Name: "web", Image: "nginx"}, {Name: "db", Image: "postgres:17"},
		}},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhaseFailed, NodeRef: "node-a", AssignmentGeneration: 4,
			Conditions: []v1.Condition{{Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: reason}},
		},
	}
}

func latch(p *v1.Project, generation int64) {
	p.Status.EnsureRecovery().WorkloadStopped = &v1.WorkloadStoppedEvidence{
		Assignment: v1.AssignmentIdentity{ProjectUID: p.UID, NodeName: p.Status.NodeRef, AssignmentGeneration: generation},
		ObservedAt: time.Now().UTC(),
	}
}

type fakeStopClient struct {
	current    *v1.Project
	getErr     error
	stoppedErr error
	onReport   func() // runs when the evidence is sent, before it is answered

	mu      sync.Mutex
	stopped []AssignmentFence
	patched []string // RecoveryBlocked reasons
	cleared int
}

func (f *fakeStopClient) NodeName() string { return "node-a" }

func (f *fakeStopClient) GetProject(context.Context, string) (*v1.Project, error) {
	return f.current, f.getErr
}

func (f *fakeStopClient) ReportWorkloadStopped(_ context.Context, _ string, fence AssignmentFence) error {
	if f.onReport != nil {
		f.onReport()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stoppedErr != nil {
		return f.stoppedErr
	}
	f.stopped = append(f.stopped, fence)
	return nil
}

func (f *fakeStopClient) PatchProjectCondition(_ context.Context, _ string, _ AssignmentFence, t v1.ConditionType, _ v1.ConditionStatus, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t == v1.ConditionTypeRecoveryBlocked {
		f.patched = append(f.patched, reason)
	}
	return nil
}

func (f *fakeStopClient) ClearProjectCondition(context.Context, string, AssignmentFence, v1.ConditionType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared++
	return nil
}

type fakeStopRuntime struct {
	stopErr error
	listErr error
	after   []docker.ProjectContainer // what Docker lists once the stop ran
	stops   int
}

func (f *fakeStopRuntime) StopAssignment(context.Context, *v1.Project) error {
	f.stops++
	return f.stopErr
}

func (f *fakeStopRuntime) ProjectContainers(context.Context, *v1.Project) ([]docker.ProjectContainer, error) {
	return f.after, f.listErr
}

func stoppedStates() []docker.ProjectContainer {
	return []docker.ProjectContainer{
		{Name: "blog-web", Service: "web", State: "exited"},
		{Name: "blog-db", Service: "db", State: "exited"},
	}
}

// otherAssignment is a container listed for the Project that belongs to a
// different assignment of it.
func otherAssignment(name, state string) docker.ProjectContainer {
	return docker.ProjectContainer{Name: name, Service: "db", State: state, NotOwned: docker.ErrContainerNotOwned}
}

func runStopFailed(client *fakeStopClient, rt *fakeStopRuntime, busy busyChecker, p *v1.Project) {
	stopFailedProjects(context.Background(), client, rt, busy, nil, []*v1.Project{p}, zap.NewNop())
}

func TestStopFailedProjects_Reasons(t *testing.T) {
	for reason, want := range map[string]bool{
		"LocalRestartExhausted": true,
		"ContainerRestartStuck": true,
		"InspectError":          true,
		"ReconcileError":        true,
		"ContainerExited":       true,
		"ContainerCrashed":      true,
		"SecretNotFound":        false,
		"SecretKeyNotFound":     false,
		"RestoreError":          false,
		"RemoveError":           false,
		"ContainerUnhealthy":    false,
		"":                      false,
	} {
		t.Run(reason, func(t *testing.T) {
			p := failedProject(reason)
			client := &fakeStopClient{current: failedProject(reason)}
			rt := &fakeStopRuntime{after: stoppedStates()}
			runStopFailed(client, rt, nil, p)
			assert.Equal(t, want, rt.stops == 1, "stopped")
			assert.Equal(t, want, len(client.stopped) == 1, "reported")
		})
	}
}

func TestStopFailedProjects_StopsThenReports(t *testing.T) {
	client := &fakeStopClient{current: failedProject("LocalRestartExhausted")}
	client.current.Status.Conditions = append(client.current.Status.Conditions,
		v1.Condition{Type: v1.ConditionTypeRecoveryBlocked, Status: v1.ConditionTrue, Reason: recoveryBlockedStopUnconfirmed})
	rt := &fakeStopRuntime{after: stoppedStates()}
	runStopFailed(client, rt, nil, failedProject("LocalRestartExhausted"))

	assert.Equal(t, 1, rt.stops)
	assert.Equal(t, []AssignmentFence{{UID: "uid-1", NodeRef: "node-a", Generation: 4}}, client.stopped,
		"the evidence names the Project UID, node and generation")
	assert.Equal(t, 1, client.cleared, "a block left by an earlier attempt is cleared")
}

func TestStopFailedProjects_Blocked(t *testing.T) {
	running := func(state string) []docker.ProjectContainer {
		return []docker.ProjectContainer{{Name: "blog-web", Service: "web", State: state}}
	}
	tests := []struct {
		name   string
		rt     *fakeStopRuntime
		reason string
	}{
		{name: "the stop fails", rt: &fakeStopRuntime{stopErr: errors.New("docker unreachable")}, reason: recoveryBlockedStopUnconfirmed},
		{name: "the check fails", rt: &fakeStopRuntime{listErr: errors.New("docker unreachable")}, reason: recoveryBlockedStopUnconfirmed},
		{name: "a container is still running", rt: &fakeStopRuntime{after: running("running")}, reason: recoveryBlockedStopUnconfirmed},
		{name: "a container is restarting", rt: &fakeStopRuntime{after: running("restarting")}, reason: recoveryBlockedStopUnconfirmed},
		{name: "a container is paused", rt: &fakeStopRuntime{after: running("paused")}, reason: recoveryBlockedStopUnconfirmed},
		{
			// The spec no longer names the service, but its container is this
			// assignment's and still a copy of the workload.
			name:   "a dropped service's container is still running",
			rt:     &fakeStopRuntime{after: append(stoppedStates(), docker.ProjectContainer{Name: "blog-worker", Service: "worker", State: "running"})},
			reason: recoveryBlockedStopUnconfirmed,
		},
		{
			name:   "another assignment's container is running",
			rt:     &fakeStopRuntime{after: append(stoppedStates(), otherAssignment("blog-old", "running"))},
			reason: recoveryBlockedStaleContainer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeStopClient{current: failedProject("LocalRestartExhausted")}
			runStopFailed(client, tt.rt, nil, failedProject("LocalRestartExhausted"))
			assert.Empty(t, client.stopped, "no evidence without a confirmed stop")
			assert.Equal(t, []string{tt.reason}, client.patched)

			// The next poll tries again.
			tt.rt.stopErr, tt.rt.listErr, tt.rt.after = nil, nil, stoppedStates()
			runStopFailed(client, tt.rt, nil, failedProject("LocalRestartExhausted"))
			assert.Len(t, client.stopped, 1, "retried on a later poll")
		})
	}
}

// A stopped leftover of another assignment says nothing runs, and is not this
// assignment's to stop; it does not hold the report back.
func TestStopFailedProjects_AStoppedLeftoverOfAnotherAssignmentDoesNotBlock(t *testing.T) {
	client := &fakeStopClient{current: failedProject("ContainerCrashed")}
	rt := &fakeStopRuntime{after: append(stoppedStates(), otherAssignment("blog-db-gen3", "exited"))}
	runStopFailed(client, rt, nil, failedProject("ContainerCrashed"))
	assert.Len(t, client.stopped, 1)
	assert.Empty(t, client.patched)
}

func TestStopFailedProjects_LocalRecord(t *testing.T) {
	stop := func(t *testing.T, latches *StopLatches, client *fakeStopClient) {
		t.Helper()
		stopFailedProjects(context.Background(), client, &fakeStopRuntime{after: stoppedStates()}, nil, latches,
			[]*v1.Project{failedProject("LocalRestartExhausted")}, zap.NewNop())
	}

	t.Run("written before the evidence is sent", func(t *testing.T) {
		latches := NewStopLatches(t.TempDir())
		client := &fakeStopClient{current: failedProject("LocalRestartExhausted")}
		var heldWhenSent bool
		client.onReport = func() { heldWhenSent, _ = latches.Holds(failedProject("LocalRestartExhausted")) }
		stop(t, latches, client)
		assert.True(t, heldWhenSent)
		assert.Len(t, client.stopped, 1)
	})

	t.Run("no record, no report", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		client := &fakeStopClient{current: failedProject("LocalRestartExhausted")}
		stop(t, NewStopLatches(file), client)
		assert.Empty(t, client.stopped, "evidence the guard does not know of is never sent")
		assert.Equal(t, []string{recoveryBlockedStopUnconfirmed}, client.patched)
	})

	t.Run("dropped when the server refuses the evidence", func(t *testing.T) {
		latches := NewStopLatches(t.TempDir())
		client := &fakeStopClient{current: failedProject("LocalRestartExhausted"), stoppedErr: ErrStaleAssignment}
		stop(t, latches, client)
		held, err := latches.Holds(failedProject("LocalRestartExhausted"))
		require.NoError(t, err)
		assert.False(t, held, "the server holds no evidence, so nothing needs guarding")
	})

	t.Run("kept while the answer is unknown", func(t *testing.T) {
		latches := NewStopLatches(t.TempDir())
		client := &fakeStopClient{current: failedProject("LocalRestartExhausted"), stoppedErr: errors.New("timeout")}
		stop(t, latches, client)
		held, err := latches.Holds(failedProject("LocalRestartExhausted"))
		require.NoError(t, err)
		assert.True(t, held, "the server may have recorded it")
	})
}

func TestStopFailedProjects_LeavesAlone(t *testing.T) {
	t.Run("already reported for this generation", func(t *testing.T) {
		p := failedProject("LocalRestartExhausted")
		latch(p, 4)
		client := &fakeStopClient{current: p}
		rt := &fakeStopRuntime{after: stoppedStates()}
		runStopFailed(client, rt, nil, p)
		assert.Zero(t, rt.stops)
		assert.Empty(t, client.stopped)
	})

	t.Run("evidence for an older generation does not count", func(t *testing.T) {
		p := failedProject("LocalRestartExhausted")
		latch(p, 3)
		client := &fakeStopClient{current: p}
		rt := &fakeStopRuntime{after: stoppedStates()}
		runStopFailed(client, rt, nil, p)
		assert.Len(t, client.stopped, 1)
	})

	changed := map[string]func(p *v1.Project){
		"reassigned":                func(p *v1.Project) { p.Status.AssignmentGeneration = 5 },
		"moved to another node":     func(p *v1.Project) { p.Status.NodeRef = "node-b" },
		"recreated":                 func(p *v1.Project) { p.UID = "uid-2" },
		"no longer Failed":          func(p *v1.Project) { p.Status.Phase = v1.ProjectPhaseTerminating },
		"failed for another reason": func(p *v1.Project) { p.Status.Conditions[0].Reason = "SecretNotFound" },
	}
	for name, mutate := range changed {
		t.Run(name+" since the poll", func(t *testing.T) {
			current := failedProject("LocalRestartExhausted")
			mutate(current)
			client := &fakeStopClient{current: current}
			rt := &fakeStopRuntime{after: stoppedStates()}
			runStopFailed(client, rt, nil, failedProject("LocalRestartExhausted"))
			assert.Zero(t, rt.stops, "nothing is stopped under a grant that moved")
			assert.Empty(t, client.stopped)
		})
	}

	t.Run("busy with another operation", func(t *testing.T) {
		coordinator := backup.NewCoordinator()
		release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: "default", Name: "blog"}, backup.OpBackup)
		require.True(t, ok)
		defer release()
		client := &fakeStopClient{current: failedProject("LocalRestartExhausted")}
		rt := &fakeStopRuntime{after: stoppedStates()}
		runStopFailed(client, rt, coordinator, failedProject("LocalRestartExhausted"))
		assert.Zero(t, rt.stops)
	})

	t.Run("the server cannot be reached", func(t *testing.T) {
		client := &fakeStopClient{getErr: errors.New("unreachable")}
		rt := &fakeStopRuntime{after: stoppedStates()}
		runStopFailed(client, rt, nil, failedProject("LocalRestartExhausted"))
		assert.Zero(t, rt.stops)
	})
}

func TestStopFailedProjects_EvidenceNotAcceptedIsRetried(t *testing.T) {
	client := &fakeStopClient{current: failedProject("LocalRestartExhausted"), stoppedErr: errors.New("server busy")}
	rt := &fakeStopRuntime{after: stoppedStates()}
	runStopFailed(client, rt, nil, failedProject("LocalRestartExhausted"))
	assert.Empty(t, client.stopped)
	assert.Empty(t, client.patched, "stopped is not blocked; only the report is retried")

	client.stoppedErr = nil
	runStopFailed(client, rt, nil, failedProject("LocalRestartExhausted"))
	assert.Len(t, client.stopped, 1)
}

// latchedRunning is a Running Project this node has reported stopped.
func latchedRunning() v1.Project {
	p := runningProject("blog")
	p.ObjectMeta.UID = "uid-1"
	p.Status.AssignmentGeneration = 2
	latch(&p, 2)
	return p
}

func TestLatchedGenerationIsNeverStarted(t *testing.T) {
	touched := func() (*mockRuntime, *bool) {
		var any bool
		return &mockRuntime{
			inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
				any = true
				return nil, nil
			},
			reconcileFn: func(context.Context, *v1.Project) error { any = true; return nil },
			startFn:     func(context.Context, *v1.Project) error { any = true; return nil },
			recoverFn:   func(context.Context, *v1.Project, []string) error { any = true; return nil },
		}, &any
	}

	t.Run("the poll loop neither health-checks nor reconciles it", func(t *testing.T) {
		for _, phase := range []v1.ProjectPhase{v1.ProjectPhaseRunning, v1.ProjectPhaseScheduled} {
			p := latchedRunning()
			p.Status.Phase = phase
			client, updates := newReconcileServer(t, []v1.Project{p})
			rt, any := touched()
			reconcileProjects(context.Background(), client, rt, nil, nil, nil, nil, newRecoveryTracker(), nil, zap.NewNop())
			assert.False(t, *any, "phase %s: Docker untouched", phase)
			assert.Empty(t, *updates)
		}
	})

	t.Run("reconcileOne refuses it", func(t *testing.T) {
		p := latchedRunning()
		p.Status.Phase = v1.ProjectPhaseScheduled
		client, _ := newReconcileServer(t, nil)
		rt, any := touched()
		reconcileOne(context.Background(), client, rt, nil, nil, &p, zap.NewNop())
		assert.False(t, *any)
	})

	t.Run("an Agent restart does not bring it back", func(t *testing.T) {
		client, _ := newReconcileServer(t, []v1.Project{latchedRunning()})
		rt, any := touched()
		bootstrapRunningProjects(context.Background(), client, rt, nil, nil, newRecoveryTracker(), zap.NewNop())
		assert.False(t, *any, "the latch is read from the server before any Docker call")
	})

	t.Run("a Project not latched is still health-checked", func(t *testing.T) {
		p := latchedRunning()
		p.Status.Recovery = nil
		client, _ := newReconcileServer(t, []v1.Project{p})
		rt, any := touched()
		reconcileProjects(context.Background(), client, rt, nil, nil, nil, nil, newRecoveryTracker(), nil, zap.NewNop())
		assert.True(t, *any)
	})
}

func TestRecoverLocallyStopsAtALatchSetSinceThePoll(t *testing.T) {
	// The poll saw the Project unlatched; by the time recovery re-reads it,
	// this node has reported it stopped. Recovery must not start it.
	polled := latchedRunning()
	polled.Status.Recovery = nil
	polled.Status.NodeRef = "test-node"
	current := polled
	latch(&current, current.Status.AssignmentGeneration)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current)
	}))
	defer server.Close()
	client := NewClient(zap.NewNop(), server.URL, "test-node")

	var recovered bool
	rt := &mockRuntime{recoverFn: func(context.Context, *v1.Project, []string) error { recovered = true; return nil }}
	bad := []serviceState{{Service: "web", Status: "exited", Health: healthStartable}}
	recoverLocally(context.Background(), client, rt, nil, newRecoveryTracker(), &polled, bad, zap.NewNop())
	assert.False(t, recovered)
}
