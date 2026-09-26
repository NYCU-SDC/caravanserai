package agent

import (
	"context"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These tests drive healthCheckOne rather than the tracker, because the bug
// they pin was not in the tracker's arithmetic but in where the reset sat:
// the attempt count was only forgotten after ten unbroken healthy minutes, so
// failures minutes apart shared one budget and the fourth one, on a Project
// that had been recovered successfully three times, went straight to Failed.
// Asserting on the tracker alone would have passed either way.

// incidentHarness runs repeated poll cycles against one Project whose
// container states the test controls.
type incidentHarness struct {
	server  *gateServer
	client  *Client
	runtime *gateRuntime
	routes  *recordingRoutes
	tracker *recoveryTracker
	clock   *fakeClock
	states  []docker.ContainerState
}

func newIncidentHarness(t *testing.T) *incidentHarness {
	t.Helper()
	h := &incidentHarness{routes: &recordingRoutes{}}
	h.server, h.client = newGateServer(t, gateProject())
	h.runtime = newGateRuntime()
	h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return h.states, nil
	}
	h.tracker, h.clock = newTestTracker()
	return h
}

// poll runs one iteration of the health check, the way the loop would.
func (h *incidentHarness) poll(t *testing.T, p *v1.Project) {
	t.Helper()
	healthCheckOne(t.Context(), h.client, h.runtime, h.routes, backup.NewCoordinator(), h.tracker, p, zap.NewNop())
	h.clock.advance(defaultPollInterval)
}

func webContainer(status string, exitCode int) []docker.ContainerState {
	return []docker.ContainerState{{
		ServiceName: "web", ContainerID: "id-web", Status: status, ExitCode: exitCode,
	}}
}

// The reported bug, as a test. Four separate failures, each recovered and each
// observed running on the following poll, are four incidents of one attempt.
// The fourth must still get a recovery of its own rather than inheriting a
// budget the first three already spent.
func TestRecoveryBudgetIsPerIncidentNotPerProject(t *testing.T) {
	p := gateProject()
	h := newIncidentHarness(t)
	key := keyForProject(p)

	for cycle := 1; cycle <= 4; cycle++ {
		// The container is stopped or removed from under cara.
		h.states = webContainer("exited", 0)
		h.poll(t, p)
		require.Equal(t, cycle, h.runtime.recoveries, "cycle %d must run a recovery of its own", cycle)
		require.Equal(t, 1, h.tracker.attempts(key), "cycle %d must be attempt 1", cycle)

		// The recovery worked, and the next poll sees it running.
		h.states = webContainer("running", 0)
		h.poll(t, p)
		require.Equal(t, 0, h.tracker.attempts(key), "cycle %d: a verified recovery closes the incident", cycle)
	}

	assert.Empty(t, h.server.updates, "no cycle may report the Project Failed")
}

// The protection the per-incident budget must not give away: a container that
// never comes back is one incident, however many polls it spans, and it still
// exhausts after maxRecoveryAttempts.
func TestUnrecoveredFailureStillExhausts(t *testing.T) {
	p := gateProject()
	h := newIncidentHarness(t)
	h.states = webContainer("exited", 1)

	// Every poll sees the same dead container. Three attempts are spent, then
	// the verification timeout has to expire before exhaustion is reported.
	for i := 0; i < maxRecoveryAttempts; i++ {
		h.poll(t, p)
	}
	require.Equal(t, maxRecoveryAttempts, h.runtime.recoveries)
	require.Empty(t, h.server.updates, "the final attempt still has its verification window")

	for h.clock.Since(h.tracker.entries[keyForProject(p)].lastAttemptAt) < recoveryVerifyTimeout {
		h.poll(t, p)
		require.Empty(t, h.server.updates, "must not give up before the verification timeout")
	}
	h.poll(t, p)

	require.Len(t, h.server.updates, 1)
	assert.Equal(t, v1.ProjectPhaseFailed, h.server.updates[0].Phase)
	assert.Equal(t, "LocalRestartExhausted", h.server.updates[0].Reason)
	assert.Equal(t, maxRecoveryAttempts, h.runtime.recoveries, "exhaustion starts no further attempt")
}

// A container that dies again before the next poll is never observed running,
// so nothing resets and the attempts accumulate. This is the case that makes a
// single healthy observation a safe signal to reset on: the crash loop that
// would be restarted forever cannot produce one.
func TestFailureBetweenPollsDoesNotResetTheBudget(t *testing.T) {
	p := gateProject()
	h := newIncidentHarness(t)
	key := keyForProject(p)

	// Each poll finds it exited: it came up after the previous recovery and
	// died again in between, so no poll ever sees it running.
	h.states = webContainer("exited", 0)
	for i := 1; i <= maxRecoveryAttempts; i++ {
		h.poll(t, p)
		require.Equal(t, i, h.tracker.attempts(key), "poll %d", i)
	}

	assert.Equal(t, maxRecoveryAttempts, h.tracker.attempts(key),
		"a container that never survives a poll interval must exhaust")
}
