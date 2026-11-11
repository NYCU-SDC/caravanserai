package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// updateProject is gateProject at spec revision 2, with revision 1 reported
// running: an update is pending.
func updateProject() *v1.Project {
	p := gateProject()
	p.SpecRevision = 2
	p.Status.ObservedSpecRevision = 1
	return p
}

// updateRuntime is a gateRuntime that serves a fixed drift and counts updates.
type updateRuntime struct {
	*gateRuntime
	drift     docker.SpecDrift
	updates   int
	updateErr error
	onUpdate  func()
	running   bool
}

func newUpdateRuntime(drift docker.SpecDrift) *updateRuntime {
	u := &updateRuntime{gateRuntime: newGateRuntime(), drift: drift, running: true}
	u.driftFn = func(context.Context, *v1.Project) (docker.SpecDrift, error) { return u.drift, nil }
	u.updateFn = func(context.Context, *v1.Project, docker.SpecDrift) error {
		u.updates++
		if u.onUpdate != nil {
			u.onUpdate()
		}
		if u.updateErr == nil {
			u.drift = docker.SpecDrift{}
		}
		return u.updateErr
	}
	u.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		if !u.running {
			return nil, nil
		}
		return []docker.ContainerState{{ServiceName: "web", Status: "running"}}, nil
	}
	return u
}

var outdatedWeb = docker.SpecDrift{Outdated: []string{"web"}}

func TestSpecUpdateRecreatesWithoutRecoveryAndReportsTheRevision(t *testing.T) {
	gs, client := newGateServer(t, updateProject())
	rt := newUpdateRuntime(outdatedWeb)
	tracker, _ := newTestTracker()
	routes := &recordingRoutes{}

	healthCheckOne(t.Context(), client, rt, routes, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())

	assert.Equal(t, 1, rt.updates)
	assert.Zero(t, rt.preflights, "the recreate is not a tier-1 recovery")
	assert.Zero(t, tracker.attempts(testKey()), "no recovery attempt is spent")
	assert.Empty(t, gs.updates, "the phase is left alone")
	reports := gs.specReports()
	require.Len(t, reports, 1)
	assert.Equal(t, int64(2), reports[0].SpecRevision)
	assert.True(t, reports[0].Applied)
	assert.Equal(t, "uid-1", reports[0].UID)
	// No ingress in this spec, so refreshing the routes clears them.
	assert.Equal(t, []string{"guestbook"}, routes.removedNames(), "routes are refreshed after the recreate")
}

func TestSpecUpdatePullFailureKeepsTheOldContainersAndWaits(t *testing.T) {
	gs, client := newGateServer(t, updateProject())
	rt := newUpdateRuntime(outdatedWeb)
	rt.updateErr = fmt.Errorf("service %q: %w %q: manifest unknown", "web", docker.ErrImagePull, "nginx:nope")
	tracker, clk := newTestTracker()
	check := func() {
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
	}

	check()
	reports := gs.specReports()
	require.Len(t, reports, 1)
	assert.False(t, reports[0].Applied)
	assert.Equal(t, v1.ReasonImagePullFailed, reports[0].Reason)
	assert.Empty(t, gs.updates, "a failed pull stopped nothing: the Project stays Running")

	check()
	assert.Equal(t, 1, rt.updates, "the next poll does not pull again right away")
	assert.Zero(t, rt.preflights, "and does not read the outdated containers as a fault")

	clk.advance(specUpdateRetryInterval)
	check()
	assert.Equal(t, 2, rt.updates, "after the interval it tries again")

	for range maxSpecUpdateAttempts + 1 {
		clk.advance(specUpdateRetryInterval)
		check()
	}
	assert.Empty(t, gs.updates, "failed pulls never end in Failed")
}

func TestSpecUpdateFailingAfterStopEndsInFailed(t *testing.T) {
	gs, client := newGateServer(t, updateProject())
	rt := newUpdateRuntime(outdatedWeb)
	rt.updateErr = errors.New("create container: no space left on device")
	tracker, clk := newTestTracker()

	for i := range maxSpecUpdateAttempts {
		if i > 0 {
			clk.advance(specUpdateRetryInterval)
		}
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
	}

	reports := gs.specReports()
	require.Len(t, reports, maxSpecUpdateAttempts)
	for _, r := range reports {
		assert.Equal(t, reasonSpecUpdateFailed, r.Reason)
	}
	require.Len(t, gs.updates, 1, "Failed only once the attempts run out")
	assert.Equal(t, v1.ProjectPhaseFailed, gs.updates[0].Phase)
	assert.Equal(t, reasonSpecUpdateFailed, gs.updates[0].Reason)
}

// An update and a backup never run at the same time: the update waits for a
// claim another operation holds, and holds its own while it runs.
func TestSpecUpdateAndBackupExcludeEachOther(t *testing.T) {
	_, client := newGateServer(t, updateProject())
	coord := backup.NewCoordinator()
	key := backup.ResourceKey{Namespace: "default", Name: "guestbook"}
	tracker, _ := newTestTracker()

	t.Run("a backup in flight holds the update off", func(t *testing.T) {
		rt := newUpdateRuntime(outdatedWeb)
		release, ok := coord.TryClaim(key, backup.OpBackup)
		require.True(t, ok)
		healthCheckOne(t.Context(), client, rt, nil, coord, tracker, updateProject(), zap.NewNop())
		release()
		assert.Zero(t, rt.updates)
		assert.Zero(t, rt.preflights, "nor is the outdated Project judged unhealthy")
	})

	t.Run("a backup cannot start while the update runs", func(t *testing.T) {
		rt := newUpdateRuntime(outdatedWeb)
		var backupClaimed bool
		rt.onUpdate = func() {
			release, ok := coord.TryClaim(key, backup.OpBackup)
			backupClaimed = ok
			if ok {
				release()
			}
		}
		healthCheckOne(t.Context(), client, rt, nil, coord, tracker, updateProject(), zap.NewNop())
		assert.Equal(t, 1, rt.updates)
		assert.False(t, backupClaimed)
		_, busy := coord.Current(key)
		assert.False(t, busy, "the claim is released afterwards")
	})
}

func TestMissingContainerIsAnUpdateOnlyWhileTheRevisionIsPending(t *testing.T) {
	missing := docker.SpecDrift{Missing: []string{"web"}}

	t.Run("pending: the update creates it", func(t *testing.T) {
		_, client := newGateServer(t, updateProject())
		rt := newUpdateRuntime(missing)
		rt.running = false
		tracker, _ := newTestTracker()
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
		assert.Equal(t, 1, rt.updates)
		assert.Zero(t, rt.preflights)
	})

	t.Run("revision running: it is a crash for tier-1", func(t *testing.T) {
		p := updateProject()
		p.Status.ObservedSpecRevision = 2
		_, client := newGateServer(t, p)
		rt := newUpdateRuntime(missing)
		rt.running = false
		tracker, _ := newTestTracker()
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, p, zap.NewNop())
		assert.Zero(t, rt.updates)
		assert.Equal(t, 1, rt.preflights, "recovery takes it")
	})
}

func TestSpecUpdateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(current *v1.Project)
	}{
		{"the assignment moved", func(c *v1.Project) { c.Status.AssignmentGeneration++ }},
		{"the Project is no longer Running", func(c *v1.Project) { c.Status.Phase = v1.ProjectPhaseTerminating }},
		{"this assignment reported its workload stopped", func(c *v1.Project) {
			c.Status.EnsureRecovery().WorkloadStopped = &v1.WorkloadStoppedEvidence{
				Assignment: v1.AssignmentIdentity{ProjectUID: c.UID, NodeName: c.Status.NodeRef, AssignmentGeneration: c.Status.AssignmentGeneration},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := updateProject()
			tc.mutate(current)
			gs, client := newGateServer(t, current)
			rt := newUpdateRuntime(outdatedWeb)
			tracker, _ := newTestTracker()
			healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
			assert.Zero(t, rt.updates)
			assert.Empty(t, gs.specReports())
		})
	}
}

func TestHealthyProjectReportsItsRevisionOnce(t *testing.T) {
	t.Run("a revision the server has not seen running", func(t *testing.T) {
		gs, client := newGateServer(t, updateProject())
		rt := newUpdateRuntime(docker.SpecDrift{})
		tracker, _ := newTestTracker()
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
		require.Len(t, gs.specReports(), 1, "only ingress changed, say: nothing to recreate, still a new revision")
		assert.True(t, gs.specReports()[0].Applied)
		assert.Zero(t, rt.updates)
	})

	t.Run("nothing to say once it is known", func(t *testing.T) {
		p := updateProject()
		p.Status.ObservedSpecRevision = 2
		gs, client := newGateServer(t, p)
		tracker, _ := newTestTracker()
		healthCheckOne(t.Context(), client, newUpdateRuntime(docker.SpecDrift{}), nil, backup.NewCoordinator(), tracker, p, zap.NewNop())
		assert.Empty(t, gs.specReports())
	})

	t.Run("not when the containers could not be compared", func(t *testing.T) {
		gs, client := newGateServer(t, updateProject())
		rt := newUpdateRuntime(docker.SpecDrift{})
		rt.driftFn = func(context.Context, *v1.Project) (docker.SpecDrift, error) {
			return docker.SpecDrift{}, errors.New("docker down")
		}
		tracker, _ := newTestTracker()
		healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, updateProject(), zap.NewNop())
		assert.Empty(t, gs.specReports())
	})
}

func TestRemovingEveryIngressRuleRemovesTheRoutes(t *testing.T) {
	p := gateProject()
	routes := &recordingRoutes{}
	updateProxyRoutes(t.Context(), &mockRuntime{}, routes, p, zap.NewNop())
	assert.Equal(t, []string{"guestbook"}, routes.removedNames())
	assert.Empty(t, routes.updated)
}
