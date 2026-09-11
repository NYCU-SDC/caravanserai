package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These tests pin the order healthCheckOne weighs mixed faults in:
//
//	paused or unknown → Failed, nothing touched
//	anything restarting → defer the whole Project, bounded by a timeout
//	otherwise → tier-1 recovery

// webAndDB is a Running two-service Project assigned to gateNode.
func webAndDB() *v1.Project {
	p := gateProject()
	p.Spec.Services = []v1.ServiceDef{
		{Name: "web", Image: "nginx:alpine"},
		{Name: "db", Image: "postgres:16"},
	}
	return p
}

// transientHarness runs healthCheckOne against a scripted set of container
// states, recording what the runtime was asked to do.
type transientHarness struct {
	t       *testing.T
	server  *gateServer
	client  *Client
	runtime *gateRuntime
	tracker *recoveryTracker
	clock   *fakeClock
	states  []docker.ContainerState

	recovered [][]string
}

func newTransientHarness(t *testing.T) *transientHarness {
	t.Helper()
	h := &transientHarness{t: t}
	h.server, h.client = newGateServer(t, webAndDB())
	h.runtime = newGateRuntime()
	h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return h.states, nil
	}
	h.runtime.recoverFn = func(_ context.Context, _ *v1.Project, services []string) error {
		h.runtime.recoveries++
		h.recovered = append(h.recovered, services)
		return nil
	}
	h.tracker, h.clock = newTestTracker()
	return h
}

// poll runs one health check of p with the given container states.
func (h *transientHarness) poll(p *v1.Project, states ...docker.ContainerState) {
	h.states = states
	healthCheckOne(h.t.Context(), h.client, h.runtime, nil, backup.NewCoordinator(), h.tracker, p, zap.NewNop())
}

func (h *transientHarness) failed() []statusUpdate {
	var out []statusUpdate
	for _, u := range h.server.updates {
		if u.Phase == v1.ProjectPhaseFailed {
			out = append(out, u)
		}
	}
	return out
}

func svc(name, status string) docker.ContainerState {
	return docker.ContainerState{ServiceName: name, ContainerID: "id-" + name, Status: status}
}

func crashed(name string) docker.ContainerState {
	return docker.ContainerState{ServiceName: name, ContainerID: "id-" + name, Status: "exited", ExitCode: 1}
}

// exited + restarting: the whole Project waits. The exited service is not
// started while its neighbour is mid-restart, nothing is reported, and no
// attempt is spent.
func TestTransientExitedPlusRestartingDefers(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, crashed("web"), svc("db", "restarting"))

	assert.Empty(t, h.failed(), "a restart in flight is not a failure")
	assert.Zero(t, h.runtime.preflights, "recovery is not even considered")
	assert.Zero(t, h.runtime.recoveries)
	assert.Equal(t, 0, h.tracker.attempts(keyForProject(p)))
}

// missing + restarting: the missing container is not recreated either.
func TestTransientMissingPlusRestartingDoesNotRecover(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, svc("db", "restarting")) // web has no container at all

	assert.Zero(t, h.runtime.recoveries, "RecoverServices must not run while Docker is mid-action")
	assert.Empty(t, h.failed())
	assert.Equal(t, 0, h.tracker.attempts(keyForProject(p)))
}

// paused + restarting: paused needs a human whatever else is happening, so it
// wins over deferral and the Project is reported Failed at once.
func TestTransientPausedWinsOverRestarting(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, svc("web", "paused"), svc("db", "restarting"))

	failed := h.failed()
	require.Len(t, failed, 1)
	assert.Equal(t, "ContainerUnhealthy", failed[0].Reason)
	assert.Contains(t, failed[0].Message, "paused")
	assert.Zero(t, h.runtime.recoveries, "a paused container is never acted on")
	assert.Equal(t, 0, h.tracker.attempts(keyForProject(p)))
}

// Once Docker settles, the next poll judges the Project afresh and recovers
// exactly what is still broken.
func TestTransientSettlesAndRecoveryProceeds(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, crashed("web"), svc("db", "restarting"))
	require.Zero(t, h.runtime.recoveries)

	h.clock.advance(10 * time.Second)
	h.poll(p, crashed("web"), svc("db", "running"))

	require.Equal(t, 1, h.runtime.recoveries)
	assert.Equal(t, []string{"web"}, h.recovered[0], "only what is still broken is recovered")
	assert.Equal(t, 1, h.tracker.attempts(keyForProject(p)))
	assert.Empty(t, h.failed())
}

// A restart that settles back to running inside the window is not a fault.
func TestTransientSettlesToHealthy(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, svc("web", "running"), svc("db", "restarting"))
	h.clock.advance(10 * time.Second)
	h.poll(p, svc("web", "running"), svc("db", "running"))

	assert.Empty(t, h.failed())
	assert.Zero(t, h.runtime.recoveries)
}

// Deferral is bounded. A container still restarting after the timeout is
// reported, rather than leaving the Project reading Running indefinitely.
func TestTransientStuckRestartingIsReportedFailed(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	// Polls at 0s, 10s, 20s: still inside the window.
	for i := 0; i < 3; i++ {
		h.poll(p, svc("web", "running"), svc("db", "restarting"))
		require.Empty(t, h.failed(), "poll %d is inside the window", i)
		h.clock.advance(10 * time.Second)
	}

	// 30s after the first sighting.
	h.poll(p, svc("web", "running"), svc("db", "restarting"))

	failed := h.failed()
	require.Len(t, failed, 1)
	assert.Equal(t, "ContainerRestartStuck", failed[0].Reason)
	assert.Contains(t, failed[0].Message, "db(restarting)")
	assert.Zero(t, h.runtime.recoveries)
	assert.Equal(t, 0, h.tracker.attempts(keyForProject(p)), "a stuck restart is not a spent attempt")
}

// The window times one continuous restart. A restart that settled and a new
// one later are separate events, and the second gets a full window.
func TestTransientWindowRestartsAfterSettling(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, svc("web", "running"), svc("db", "restarting"))
	h.clock.advance(25 * time.Second)
	h.poll(p, svc("web", "running"), svc("db", "running")) // settled

	h.clock.advance(10 * time.Second)
	h.poll(p, svc("web", "running"), svc("db", "restarting")) // a new restart
	h.clock.advance(20 * time.Second)
	h.poll(p, svc("web", "running"), svc("db", "restarting"))

	assert.Empty(t, h.failed(), "20s into the second restart is inside its own window")
}

// A new assignment is a new grant of ownership: it does not inherit a timer
// the previous one had nearly run out.
func TestTransientTimerIsNotInheritedAcrossAssignments(t *testing.T) {
	h := newTransientHarness(t)
	gen7 := webAndDB()

	h.poll(gen7, svc("web", "running"), svc("db", "restarting"))
	h.clock.advance(25 * time.Second)

	gen8 := webAndDB()
	gen8.Status.AssignmentGeneration = 8
	h.poll(gen8, svc("web", "running"), svc("db", "restarting"))
	h.clock.advance(10 * time.Second)
	h.poll(gen8, svc("web", "running"), svc("db", "restarting"))

	assert.Empty(t, h.failed(), "gen 8 is 10s into its own window, not 35s into gen 7's")

	recreated := webAndDB()
	recreated.UID = "uid-2"
	h.poll(recreated, svc("web", "running"), svc("db", "restarting"))
	assert.Empty(t, h.failed(), "a recreated Project starts its own window too")
}

// A backup stops containers on purpose. A restart that straddles it has not
// been stuck for the length of the backup, so Maintenance resets the window.
func TestTransientWindowResetsDuringMaintenance(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()

	h.poll(p, svc("web", "running"), svc("db", "restarting"))
	h.clock.advance(25 * time.Second)

	underMaintenance := webAndDB()
	underMaintenance.Status.Conditions = []v1.Condition{{
		Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, LastTransitionTime: time.Now(),
	}}
	h.poll(underMaintenance, svc("web", "exited"), svc("db", "restarting"))
	require.Empty(t, h.failed(), "nothing is judged under maintenance")

	h.clock.advance(10 * time.Second)
	h.poll(p, svc("web", "running"), svc("db", "restarting"))

	assert.Empty(t, h.failed(), "the window started over after maintenance")
}

// The stuck timer is dropped for Projects that leave the node, the same as
// attempt state.
func TestTransientStateIsDroppedByRetain(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	require.False(t, tr.observeTransient(key))
	clk.advance(transientObservationTimeout)
	tr.retain(map[recoveryKey]struct{}{})

	assert.False(t, tr.observeTransient(key), "a dropped key starts a fresh window")
}

// ── The restart timer never outlives a judgement ─────────────────────────────

// timing reports whether the tracker is holding a restart timer for key.
func timing(tr *recoveryTracker, key recoveryKey) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	_, ok := tr.transientSince[key]
	return ok
}

// startTimer puts a restart timer on p, as a first sighting of a restarting
// container would.
func (h *transientHarness) startTimer(p *v1.Project) {
	h.t.Helper()
	h.poll(p, svc("web", "running"), svc("db", "restarting"))
	require.True(h.t, timing(h.tracker, keyForProject(p)), "setup: the timer should be running")
	h.clock.advance(10 * time.Second)
}

func underMaintenance(p *v1.Project) *v1.Project {
	p.Status.Conditions = []v1.Condition{{
		Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, LastTransitionTime: time.Now(),
	}}
	return p
}

// Maintenance comes before everything, the Docker inspect included: nothing
// about the containers is judged, so an inspect that would have failed is
// neither run nor reported.
func TestMaintenanceIsCheckedBeforeInspect(t *testing.T) {
	h := newTransientHarness(t)
	p := webAndDB()
	h.startTimer(p)

	inspected := 0
	h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		inspected++
		return nil, errors.New("docker daemon unreachable")
	}

	healthCheckOne(t.Context(), h.client, h.runtime, nil, backup.NewCoordinator(), h.tracker,
		underMaintenance(webAndDB()), zap.NewNop())

	assert.Zero(t, inspected, "InspectProject is not called under maintenance")
	assert.Empty(t, h.failed(), "no InspectError during maintenance")
	assert.False(t, timing(h.tracker, keyForProject(p)), "maintenance resets the restart timer")
}

// Each path that reports Failed drops the restart timer. Failed is terminal,
// and a Failed Project still assigned here survives retain, so a timer left
// behind would sit in memory — and be waiting, nearly expired, if the Project
// were ever brought back to Running under the same assignment.
func TestFailedPathsClearTheRestartTimer(t *testing.T) {
	t.Run("InspectError", func(t *testing.T) {
		h := newTransientHarness(t)
		p := webAndDB()
		h.startTimer(p)
		h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			return nil, errors.New("docker daemon unreachable")
		}

		healthCheckOne(t.Context(), h.client, h.runtime, nil, backup.NewCoordinator(), h.tracker, p, zap.NewNop())

		require.Len(t, h.failed(), 1)
		assert.Equal(t, "InspectError", h.failed()[0].Reason)
		assert.False(t, timing(h.tracker, keyForProject(p)))
	})

	t.Run("paused", func(t *testing.T) {
		h := newTransientHarness(t)
		p := webAndDB()
		h.startTimer(p)

		h.poll(p, svc("web", "paused"), svc("db", "restarting"))

		require.Len(t, h.failed(), 1)
		assert.False(t, timing(h.tracker, keyForProject(p)))
	})

	t.Run("unknown", func(t *testing.T) {
		h := newTransientHarness(t)
		p := webAndDB()
		h.startTimer(p)

		h.poll(p, svc("web", "something-new"), svc("db", "restarting"))

		require.Len(t, h.failed(), 1)
		assert.False(t, timing(h.tracker, keyForProject(p)))
	})

	t.Run("ContainerRestartStuck", func(t *testing.T) {
		h := newTransientHarness(t)
		p := webAndDB()
		h.startTimer(p)
		h.clock.advance(transientObservationTimeout)

		h.poll(p, svc("web", "running"), svc("db", "restarting"))

		require.Len(t, h.failed(), 1)
		assert.Equal(t, "ContainerRestartStuck", h.failed()[0].Reason)
		assert.False(t, timing(h.tracker, keyForProject(p)))
	})
}
