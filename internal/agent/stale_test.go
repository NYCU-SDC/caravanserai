package agent

import (
	"context"
	"errors"
	"fmt"
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

// ── Scheduled: reconcileOne (CARA-93) ────────────────────────────────────────

// scheduledIngressProject is ingressProject still in phase Scheduled — assigned
// to this node, not yet reported Running.
func scheduledIngressProject() *v1.Project {
	p := ingressProject()
	p.Status.Phase = v1.ProjectPhaseScheduled
	return p
}

func (h *staleHarness) reconcile(t *testing.T, p *v1.Project) (reconciled int) {
	h.runtime.reconcileFn = func(context.Context, *v1.Project) error {
		reconciled++
		return nil
	}
	reconcileOne(t.Context(), h.client, h.runtime, h.routes, nil, p, zap.NewNop())
	return reconciled
}

// Generation 6's container is running when generation 7 is scheduled here. It
// does not satisfy generation 7: no Running report, no routes, no reconcile —
// which is where its failure path used to remove the container.
func TestScheduledStaleRunningContainerIsNotAdopted(t *testing.T) {
	p := scheduledIngressProject()
	h := newStaleHarness(t, scheduledIngressProject(), staleGeneration("running"))

	reconciled := h.reconcile(t, p)

	assert.Empty(t, h.server.updates, "not reported Running — the phase is left as it is")
	assert.Empty(t, h.routes.updated, "proxy routes must not be pointed at generation 6")
	assert.Zero(t, reconciled, "ReconcileProject, and its rollback, must not run")

	patches := h.server.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "StaleContainer", patches[0].Reason)
}

// A stale container that exited with an error is not this Project's failure.
func TestScheduledStaleCrashedContainerDoesNotFailTheProject(t *testing.T) {
	p := scheduledIngressProject()
	stale := staleGeneration("exited")
	stale.ExitCode = 1
	h := newStaleHarness(t, scheduledIngressProject(), stale)

	reconciled := h.reconcile(t, p)

	assert.Empty(t, h.server.updates, "not reported Failed/ContainerExited on generation 6's behalf")
	assert.Zero(t, reconciled)
	require.Len(t, h.server.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
}

// The regression the check must not cause: generation 7's own running
// container still takes the Project to Running.
func TestScheduledOwnedRunningContainerStillReportsRunning(t *testing.T) {
	p := scheduledIngressProject()
	owned := docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"}
	h := newStaleHarness(t, scheduledIngressProject(), owned)

	reconciled := h.reconcile(t, p)

	require.Len(t, h.server.updates, 1)
	assert.Equal(t, v1.ProjectPhaseRunning, h.server.updates[0].Phase)
	assert.Equal(t, []string{"guestbook"}, h.routes.updated)
	assert.Zero(t, reconciled, "nothing to create")
	assert.Empty(t, h.server.conditionWrites)
}

// ── A service the spec no longer declares (CARA-93 review) ───────────────────

// droppedWorker is what generation 6 left behind: a container for a service
// the current spec does not declare at all, so no per-service check sees it.
func droppedWorker() docker.StaleContainer {
	return docker.StaleContainer{
		Name: "guestbook-worker", Service: "worker", ID: "id-worker",
		Reason: &docker.ContainerNotOwnedError{
			Container: "guestbook-worker", Service: "worker",
			Label: "cara.generation", Want: "7", Got: "6",
		},
	}
}

// withDroppedWorker makes the runtime report that leftover, while every
// service the spec does declare looks perfectly healthy.
func (h *staleHarness) withDroppedWorker() {
	h.runtime.staleFn = func(context.Context, *v1.Project) ([]docker.StaleContainer, error) {
		return []docker.StaleContainer{droppedWorker()}, nil
	}
}

// Scheduled: generation 7's spec dropped "worker", generation 6's worker is
// still running, and every service generation 7 declares is absent. Without
// the whole-Project listing the Project would start and run beside it.
func TestScheduledBlocksOnAContainerOfADroppedService(t *testing.T) {
	p := scheduledIngressProject()
	h := newStaleHarness(t, scheduledIngressProject(),
		docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"})
	h.withDroppedWorker()

	reconciled := h.reconcile(t, p)

	assert.Zero(t, reconciled, "generation 7 must not start beside generation 6's worker")
	assert.Empty(t, h.server.updates, "not reported Running")
	assert.Empty(t, h.routes.updated)
	patches := h.server.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "StaleContainer", patches[0].Reason)
	assert.Contains(t, patches[0].Message, `"worker"`, "the condition names the dropped service")
}

// Running: the same leftover blocks the health check, even though every
// service in the spec is running and would otherwise read as healthy.
func TestRunningBlocksOnAContainerOfADroppedService(t *testing.T) {
	p := ingressProject()
	h := newStaleHarness(t, ingressProject(),
		docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"})
	h.withDroppedWorker()

	h.check(t, p)

	require.Len(t, h.server.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
	assert.Empty(t, h.routes.updated, "a Project running beside another generation is not healthy")
	assert.Empty(t, h.server.writes("delete", v1.ConditionTypeRecoveryBlocked))
	assert.Zero(t, h.runtime.recoveries)
}

// A Docker listing failure fails closed. Inspecting by name can succeed while
// the list call fails, and that combination is exactly the one that hides a
// container whose service the spec no longer declares — so nothing is judged,
// nothing is started, and no route is pointed anywhere until the question can
// be answered again.
func TestStaleListingFailureStopsTheTick(t *testing.T) {
	listErr := func(h *staleHarness) *int {
		inspected := 0
		h.runtime.staleFn = func(context.Context, *v1.Project) ([]docker.StaleContainer, error) {
			return nil, errors.New("docker daemon unreachable")
		}
		h.runtime.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			inspected++
			return []docker.ContainerState{{ServiceName: "web", ContainerID: "id-web", Status: "running"}}, nil
		}
		return &inspected
	}

	t.Run("running", func(t *testing.T) {
		p := ingressProject()
		h := newStaleHarness(t, ingressProject(),
			docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"})
		inspected := listErr(h)

		h.check(t, p)

		assert.Zero(t, *inspected, "nothing is inspected while the question is unanswered")
		assert.Empty(t, h.routes.updated, "routes must not be pointed at containers of unknown ownership")
		assert.Empty(t, h.server.updates, "the phase is untouched — this is not a failure")
		assert.Empty(t, h.server.conditionWrites, "an unreachable daemon is not a stale container")
		assert.Zero(t, h.runtime.recoveries)
	})

	t.Run("scheduled", func(t *testing.T) {
		p := scheduledIngressProject()
		h := newStaleHarness(t, scheduledIngressProject(),
			docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"})
		inspected := listErr(h)

		reconciled := h.reconcile(t, p)

		assert.Zero(t, *inspected)
		assert.Zero(t, reconciled, "a second generation must not be started on an unanswered question")
		assert.Empty(t, h.routes.updated)
		assert.Empty(t, h.server.updates)
		assert.Empty(t, h.server.conditionWrites)
	})
}

// The tick resumes on its own once Docker answers again.
func TestStaleListingRecoversOnTheNextTick(t *testing.T) {
	p := ingressProject()
	h := newStaleHarness(t, ingressProject(),
		docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "running"})

	failing := true
	h.runtime.staleFn = func(context.Context, *v1.Project) ([]docker.StaleContainer, error) {
		if failing {
			return nil, errors.New("docker daemon unreachable")
		}
		return nil, nil
	}

	h.check(t, p)
	require.Empty(t, h.routes.updated)

	failing = false
	h.check(t, p)

	assert.Equal(t, []string{"guestbook"}, h.routes.updated, "a healthy Project is judged again once Docker answers")
}

// A stale container that appears between the check and the reconcile is
// blocked, not Failed. Failed is terminal for the poll loop, so the Project
// would never be reconciled again even after the container is removed.
func TestReconcileRefusalIsBlockedNotFailed(t *testing.T) {
	p := scheduledIngressProject()
	h := newStaleHarness(t, scheduledIngressProject(),
		docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "exited"})
	h.runtime.reconcileFn = func(context.Context, *v1.Project) error {
		return fmt.Errorf("refuse to reconcile: %w", &docker.ContainerNotOwnedError{
			Container: "guestbook-web", Service: "web", Label: "cara.generation", Want: "7", Got: "6",
		})
	}

	reconcileOne(t.Context(), h.client, h.runtime, h.routes, nil, p, zap.NewNop())

	assert.Empty(t, h.server.updates, "no Failed/ReconcileError — the Project must stay reconcilable")
	patches := h.server.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "StaleContainer", patches[0].Reason)
}

// Any other reconcile failure is still a Failed/ReconcileError.
func TestReconcileErrorIsStillFailed(t *testing.T) {
	p := scheduledIngressProject()
	h := newStaleHarness(t, scheduledIngressProject(),
		docker.ContainerState{ServiceName: "web", ContainerID: "id-web", Status: "exited"})
	h.runtime.reconcileFn = func(context.Context, *v1.Project) error {
		return errors.New("pull image: manifest unknown")
	}

	reconcileOne(t.Context(), h.client, h.runtime, h.routes, nil, p, zap.NewNop())

	require.Len(t, h.server.updates, 1)
	assert.Equal(t, v1.ProjectPhaseFailed, h.server.updates[0].Phase)
	assert.Equal(t, "ReconcileError", h.server.updates[0].Reason)
	assert.Empty(t, h.server.conditionWrites)
}
