package agent

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A container found by name can belong to another assignment: generation 6
// left it running, and the Project is now generation 7. These tests pin that
// the health check never mistakes it for this assignment's container.

// ingressProject is gateProject with an ingress rule, so a healthy check has
// proxy routes to update — which is exactly what must not happen for a stale
// container.
func ingressProject() *v1.Project {
	p := gateProject()
	p.Spec.Ingress = []v1.IngressDef{{
		Name:   "web",
		Host:   "guestbook",
		Target: v1.IngressTarget{Service: "web", Port: 80},
	}}
	return p
}

func staleGeneration(status string) docker.ContainerState {
	return docker.ContainerState{
		ServiceName: "web", ContainerID: "id-web", Status: status,
		NotOwned: &docker.ContainerNotOwnedError{
			Container: "guestbook-web", Service: "web", Label: "cara.generation", Want: "7", Got: "6",
		},
	}
}

type staleHarness struct {
	server  *gateServer
	client  *Client
	runtime *gateRuntime
	routes  *recordingRoutes
	tracker *recoveryTracker
	clock   *fakeClock
}

func newStaleHarness(t *testing.T, current *v1.Project, state docker.ContainerState) *staleHarness {
	t.Helper()
	h := &staleHarness{routes: &recordingRoutes{}}
	h.server, h.client = newGateServer(t, current)
	h.runtime = newGateRuntime()
	h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{state}, nil
	}
	h.tracker, h.clock = newTestTracker()
	return h
}

func (h *staleHarness) check(t *testing.T, p *v1.Project) {
	healthCheckOne(t.Context(), h.client, h.runtime, h.routes, backup.NewCoordinator(), h.tracker, p, zap.NewNop())
}

// Generation 6's container is running; the Project is generation 7. It is not
// a healthy Project: it is blocked, and nothing about it is touched.
func TestStaleRunningContainerIsBlockedNotHealthy(t *testing.T) {
	p := ingressProject()
	h := newStaleHarness(t, ingressProject(), staleGeneration("running"))

	h.check(t, p)

	patches := h.server.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "StaleContainer", patches[0].Reason)
	assert.Equal(t, `service "web" has a container left by another assignment, which cara will neither start nor remove`,
		patches[0].Message)
	assert.NotContains(t, patches[0].Message, "generation", "label detail stays in the agent log")

	assert.Empty(t, h.routes.updated, "proxy routes must not be pointed at a stale container")
	assert.Zero(t, h.runtime.preflights, "no recovery is attempted")
	assert.Zero(t, h.runtime.recoveries, "no Docker call")
	assert.Empty(t, h.server.updates, "the phase is not touched")
	assert.Equal(t, 0, h.tracker.attempts(keyForProject(p)))
}

// A stale container that has exited is not this assignment's to start either.
func TestStaleExitedContainerIsBlockedNotRecovered(t *testing.T) {
	p := ingressProject()
	h := newStaleHarness(t, ingressProject(), staleGeneration("exited"))

	h.check(t, p)

	require.Len(t, h.server.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
	assert.Zero(t, h.runtime.preflights)
	assert.Zero(t, h.runtime.recoveries)
	assert.Empty(t, h.server.updates, "not reported Failed either — it is blocked, not broken")
}

// An existing block is never cleared on the way: the healthy path's clear is
// exactly what a stale running container used to trigger. A different block is
// replaced by one PATCH, and the same block is not rewritten.
func TestStaleContainerNeverClearsTheCondition(t *testing.T) {
	t.Run("replaces a different block without clearing it", func(t *testing.T) {
		p := ingressProject()
		p.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
		h := newStaleHarness(t, ingressProject(), staleGeneration("running"))

		h.check(t, p)

		assert.Empty(t, h.server.writes("delete", v1.ConditionTypeRecoveryBlocked))
		patches := h.server.writes("patch", v1.ConditionTypeRecoveryBlocked)
		require.Len(t, patches, 1)
		assert.Equal(t, "StaleContainer", patches[0].Reason)
	})

	t.Run("does not rewrite the same block", func(t *testing.T) {
		p := ingressProject()
		p.Status.Conditions = []v1.Condition{{
			Type:    v1.ConditionTypeRecoveryBlocked,
			Status:  v1.ConditionTrue,
			Reason:  "StaleContainer",
			Message: `service "web" has a container left by another assignment, which cara will neither start nor remove`,
		}}
		h := newStaleHarness(t, ingressProject(), staleGeneration("running"))

		h.check(t, p)

		assert.Empty(t, h.server.conditionWrites)
	})
}

// A stale container ends any restart deferral, like every other judgement.
func TestStaleContainerClearsTheRestartTimer(t *testing.T) {
	p := ingressProject()
	h := newStaleHarness(t, ingressProject(), staleGeneration("running"))
	require.False(t, h.tracker.observeTransient(keyForProject(p)))
	h.clock.advance(10 * time.Second)

	h.check(t, p)

	assert.False(t, timing(h.tracker, keyForProject(p)))
}

// Maintenance still comes first: nothing is judged, the stale container
// included.
func TestStaleContainerUnderMaintenanceIsNotJudged(t *testing.T) {
	p := underMaintenance(ingressProject())
	h := newStaleHarness(t, ingressProject(), staleGeneration("running"))

	h.check(t, p)

	assert.Empty(t, h.server.conditionWrites)
}

// The regression the check must not cause: this assignment's own running
// container is still healthy, routes are updated, and a leftover block is
// cleared.
func TestOwnedRunningContainerIsStillHealthy(t *testing.T) {
	p := ingressProject()
	p.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
	owned := docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"}
	h := newStaleHarness(t, ingressProject(), owned)

	h.check(t, p)

	assert.Equal(t, []string{"guestbook"}, h.routes.updated)
	assert.Len(t, h.server.writes("delete", v1.ConditionTypeRecoveryBlocked), 1)
	assert.Empty(t, h.server.writes("patch", v1.ConditionTypeRecoveryBlocked))
}
