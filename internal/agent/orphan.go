package agent

import (
	"context"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// orphanGracePeriod is how long a project must stay absent from the server's
// complete ownership snapshot before its local resources are deleted. Its
// containers are stopped immediately when ownership loss is first confirmed.
//
// It mirrors runningGracePeriod in internal/server/controller/project_rescheduler.go,
// which gives a stranded Running project the same 3 minutes before acting. The
// value is deliberately far longer than the 10s poll interval: the sweep is
// destructive and irreversible for Ephemeral data, so it waits until a project's
// absence is a settled fact rather than a single observation.
const orphanGracePeriod = 3 * time.Minute

// clock abstracts time so the grace period can be tested without sleeping.
// It mirrors controller.Clock rather than importing it, keeping the agent free
// of a dependency on the server package.
type clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// realClock delegates to the standard time package.
type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

type orphanState struct {
	stopped   bool
	lostSince time.Time
}

type orphanObservation struct {
	toStop     []docker.ProjectIdentity
	toRemove   []docker.ProjectIdentity
	reappeared []docker.ProjectIdentity
}

// orphanTracker records projects whose ownership moved away from this node.
//
// State is in-process only. An agent restart forgets every candidate, which is
// conservative for deletion: the restarted agent must observe ownership loss,
// stop the containers, then confirm that loss for a fresh grace period.
//
// A tracker is not safe for concurrent use; it is owned by the single poll loop
// goroutine.
type orphanTracker struct {
	clock  clock
	states map[docker.ProjectIdentity]orphanState
}

func newOrphanTracker(c clock) *orphanTracker {
	if c == nil {
		c = realClock{}
	}
	return &orphanTracker{clock: c, states: make(map[docker.ProjectIdentity]orphanState)}
}

// observe folds one successful Docker snapshot and one successful server
// ownership snapshot into the two-stage state machine.
func (t *orphanTracker) observe(
	local []docker.ProjectIdentity,
	assigned map[docker.ProjectIdentity]*v1.Project,
) orphanObservation {
	now := t.clock.Now()
	localSet := make(map[docker.ProjectIdentity]struct{}, len(local))
	for _, project := range local {
		localSet[project] = struct{}{}
	}

	var observation orphanObservation
	for project := range localSet {
		if _, stillOurs := assigned[project]; stillOurs {
			if state, tracked := t.states[project]; tracked && state.stopped {
				observation.reappeared = append(observation.reappeared, project)
			} else {
				delete(t.states, project)
			}
			continue
		}

		state, tracked := t.states[project]
		if !tracked {
			t.states[project] = orphanState{}
			observation.toStop = append(observation.toStop, project)
			continue
		}
		if !state.stopped {
			observation.toStop = append(observation.toStop, project)
			continue
		}
		if state.lostSince.IsZero() {
			state.lostSince = now
			t.states[project] = state
			continue
		}
		if t.clock.Since(state.lostSince) >= orphanGracePeriod {
			observation.toRemove = append(observation.toRemove, project)
		}
	}

	for project, state := range t.states {
		if _, localExists := localSet[project]; localExists {
			continue
		}
		if _, stillOurs := assigned[project]; stillOurs {
			if state.stopped {
				observation.reappeared = append(observation.reappeared, project)
			} else {
				delete(t.states, project)
			}
			continue
		}
		// Keep stopped projects until every labelled resource is removed. This
		// lets a later tick retry a network or volume failure even after all
		// containers were removed successfully.
		if !state.stopped {
			delete(t.states, project)
			continue
		}
		if state.lostSince.IsZero() {
			state.lostSince = now
			t.states[project] = state
			continue
		}
		if t.clock.Since(state.lostSince) >= orphanGracePeriod {
			observation.toRemove = append(observation.toRemove, project)
		}
	}

	return observation
}

func (t *orphanTracker) markStopped(project docker.ProjectIdentity) {
	t.states[project] = orphanState{stopped: true, lostSince: t.clock.Now()}
}

func (t *orphanTracker) resetGrace() {
	for project, state := range t.states {
		state.lostSince = time.Time{}
		t.states[project] = state
	}
}

func (t *orphanTracker) forget(project docker.ProjectIdentity) { delete(t.states, project) }

func (t *orphanTracker) pending() int { return len(t.states) }

// sweepOrphans stops and eventually removes resources belonging to projects
// the server no longer assigns to this node.
//
// projects must come from one successful ListProjectsAssignedToNode call and
// must not be phase-filtered. Unknown ownership never reaches this function;
// the caller resets deletion grace instead. Reusing the same snapshot for the
// whole tick avoids acting on two inconsistent views of assignment.
func sweepOrphans(
	ctx context.Context,
	runtime docker.Runtime,
	routes RouteUpdater,
	tracker *orphanTracker,
	busy busyChecker,
	projects []*v1.Project,
	logger *zap.Logger,
) {
	localProjects, err := runtime.ListLocalProjects(ctx)
	if err != nil {
		tracker.resetGrace()
		logger.Warn("Orphan sweep: failed to list local projects", zap.Error(err))
		return
	}

	assigned := make(map[docker.ProjectIdentity]*v1.Project, len(projects))
	for _, p := range projects {
		assigned[projectIdentity(p)] = p
	}

	observation := tracker.observe(localProjects, assigned)

	for _, project := range observation.reappeared {
		serverProject := assigned[project]
		log := orphanLogger(logger, project)
		if serverProject == nil {
			continue
		}
		switch serverProject.Status.Phase {
		case v1.ProjectPhaseScheduled, v1.ProjectPhaseRunning:
			release, ok := claimOrphanCleanup(busy, project)
			if !ok {
				continue
			}
			if err := runtime.ReconcileProject(ctx, serverProject); err != nil {
				release()
				log.Error("Failed to recover project whose ownership returned", zap.Error(err))
				continue
			}
			updateProxyRoutes(ctx, runtime, routes, serverProject, log)
			release()
			log.Info("Recovered project after ownership returned to this node")
		default:
			// Failed, Pending, Terminating, and Terminated projects must not be
			// started implicitly. Their ownership is still known, so cancel only
			// the orphan cleanup state and let normal phase handling decide.
			log.Info("Cancelled orphan cleanup after ownership returned",
				zap.String("phase", string(serverProject.Status.Phase)))
		}
		tracker.forget(project)
	}

	for _, project := range observation.toStop {
		log := orphanLogger(logger, project)
		release, ok := claimOrphanCleanup(busy, project)
		if !ok {
			continue
		}
		if err := runtime.StopOrphanProject(ctx, project); err != nil {
			release()
			log.Error("Failed to stop project after ownership moved away", zap.Error(err))
			continue
		}
		if routes != nil {
			routes.Remove(project.Name)
		}
		tracker.markStopped(project)
		release()
		log.Info("Stopped project after ownership moved away; deletion grace period started",
			zap.Duration("gracePeriod", orphanGracePeriod))
	}

	for _, project := range observation.toRemove {
		log := orphanLogger(logger, project)
		release, ok := claimOrphanCleanup(busy, project)
		if !ok {
			continue
		}
		if err := runtime.RemoveOrphanProject(ctx, project); err != nil {
			release()
			log.Error("Failed to remove orphaned project", zap.Error(err))
			continue
		}
		release()
		tracker.forget(project)
		log.Info("Removed orphaned project after continuously confirmed ownership loss",
			zap.Duration("gracePeriod", orphanGracePeriod))
	}
}

func projectIdentity(project *v1.Project) docker.ProjectIdentity {
	namespace := project.Namespace
	if namespace == "" {
		namespace = v1.DefaultNamespace
	}
	return docker.ProjectIdentity{Namespace: namespace, Name: project.Name}
}

func claimOrphanCleanup(busy busyChecker, project docker.ProjectIdentity) (func(), bool) {
	if busy == nil {
		return func() {}, true
	}
	key := backup.ResourceKey{Namespace: project.Namespace, Name: project.Name}
	return busy.TryClaim(key, backup.OpOrphanCleanup)
}

func orphanLogger(logger *zap.Logger, project docker.ProjectIdentity) *zap.Logger {
	return logger.With(
		zap.String("namespace", project.Namespace),
		zap.String("project", project.Name),
	)
}
