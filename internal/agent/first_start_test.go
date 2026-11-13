package agent

import (
	"context"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// scheduledProject is gateProject while it is still being placed here.
func scheduledProject() *v1.Project {
	p := gateProject()
	p.Status.Phase = v1.ProjectPhaseScheduled
	return p
}

// webExitedOnStart is a first start where web exited with an error, as an app
// does when it starts before its database is ready.
func webExitedOnStart(rt *gateRuntime) {
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{{ServiceName: "web", ContainerID: "c1", Status: "exited", ExitCode: 1}}, nil
	}
}

func failedReports(gs *gateServer) []statusUpdate {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var out []statusUpdate
	for _, u := range gs.updates {
		if u.Phase == v1.ProjectPhaseFailed {
			out = append(out, u)
		}
	}
	return out
}

// A container that exits on its first start is restarted in place, as a
// Running Project's would be, instead of failing the Project at once. Failing
// it at once handed a start-up race to Tier-2, which stops the whole Project
// and moves it.
func TestFirstStartExitIsRestartedInPlace(t *testing.T) {
	gs, client := newGateServer(t, scheduledProject())
	rt := newGateRuntime()
	webExitedOnStart(rt)
	tracker, _ := newTestTracker()

	reconcileOne(t.Context(), client, rt, nil, nil, tracker, scheduledProject(), zap.NewNop())

	assert.Equal(t, 1, rt.recoveries, "the exited service is restarted")
	assert.Empty(t, failedReports(gs), "not reported Failed on the first exit")
}

// The restarts are tier-1's, with its budget: once they are used up the
// Project is Failed with LocalRestartExhausted, which Tier-2 may then move.
func TestFirstStartExitExhaustsLikeTier1(t *testing.T) {
	gs, client := newGateServer(t, scheduledProject())
	rt := newGateRuntime()
	webExitedOnStart(rt)
	tracker, clk := newTestTracker()

	for range 10 {
		reconcileOne(t.Context(), client, rt, nil, nil, tracker, scheduledProject(), zap.NewNop())
		if len(failedReports(gs)) > 0 {
			break
		}
		clk.now = clk.now.Add(time.Minute)
	}

	assert.Equal(t, maxRecoveryAttempts, rt.recoveries)
	reports := failedReports(gs)
	require.Len(t, reports, 1)
	assert.Equal(t, "LocalRestartExhausted", reports[0].Reason)
}

// Without tier-1 recovery the first exit is reported as before.
func TestFirstStartExitWithoutRecoveryFails(t *testing.T) {
	gs, client := newGateServer(t, scheduledProject())
	rt := newGateRuntime()
	webExitedOnStart(rt)

	reconcileOne(t.Context(), client, rt, nil, nil, nil, scheduledProject(), zap.NewNop())

	assert.Zero(t, rt.recoveries)
	reports := failedReports(gs)
	require.Len(t, reports, 1)
	assert.Equal(t, "ContainerExited", reports[0].Reason)
}

// The re-read before a first-start restart is fenced on the phase too: a
// Project that became Running meanwhile is the health check's to judge.
func TestFirstStartRestartIsFencedOnPhase(t *testing.T) {
	running := scheduledProject()
	running.Status.Phase = v1.ProjectPhaseRunning
	gs, client := newGateServer(t, running)
	rt := newGateRuntime()
	webExitedOnStart(rt)
	tracker, _ := newTestTracker()

	reconcileOne(t.Context(), client, rt, nil, nil, tracker, scheduledProject(), zap.NewNop())

	assert.Zero(t, rt.recoveries)
	assert.Empty(t, failedReports(gs))
}
