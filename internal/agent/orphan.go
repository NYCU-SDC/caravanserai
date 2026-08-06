package agent

import (
	"context"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// orphanGracePeriod is how long a project must stay absent from the server's
// reconcile response before its local containers are removed.
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

// orphanTracker records when each project was first seen without a matching
// entry in the server's reconcile response.
//
// State is in-process only. An agent restart forgets every candidate, which is
// the conservative direction: a restarted agent re-observes the absence for a
// full grace period before removing anything, matching how the agent rebuilds
// all its other state from the server after a restart (bootstrapRunningProjects).
//
// A tracker is not safe for concurrent use; it is owned by the single poll loop
// goroutine.
type orphanTracker struct {
	clock clock
	// firstSeen maps project name → the time it was first observed missing.
	firstSeen map[string]time.Time
}

func newOrphanTracker(c clock) *orphanTracker {
	if c == nil {
		c = realClock{}
	}
	return &orphanTracker{clock: c, firstSeen: make(map[string]time.Time)}
}

// observe folds one round of observations into the tracker and returns the
// projects whose grace period has fully elapsed.
//
// localNames is every project with containers on this host; assigned is the set
// of project names the server still assigns here. A project that reappears in
// assigned has its clock discarded, so an intermittent absence never
// accumulates toward removal across separate episodes.
func (t *orphanTracker) observe(localNames []string, assigned map[string]struct{}) []string {
	now := t.clock.Now()

	missing := make(map[string]struct{}, len(localNames))
	var expired []string

	for _, name := range localNames {
		if _, stillOurs := assigned[name]; stillOurs {
			continue
		}
		missing[name] = struct{}{}

		first, tracked := t.firstSeen[name]
		if !tracked {
			// First observation only starts the clock; removal needs a later
			// round to confirm the absence persisted.
			t.firstSeen[name] = now
			continue
		}
		if t.clock.Since(first) >= orphanGracePeriod {
			expired = append(expired, name)
		}
	}

	// Drop candidates that are no longer missing: either the server assigns
	// them here again, or their containers are gone. Both mean the recorded
	// timestamp describes a situation that no longer exists.
	for name := range t.firstSeen {
		if _, still := missing[name]; !still {
			delete(t.firstSeen, name)
		}
	}

	return expired
}

// forget clears a project's candidacy, used once it has been removed so a
// future project of the same name starts its own clock.
func (t *orphanTracker) forget(name string) {
	delete(t.firstSeen, name)
}

// pending reports how many projects are currently counting toward removal.
// Exposed for tests and diagnostics.
func (t *orphanTracker) pending() int { return len(t.firstSeen) }

// sweepOrphans removes containers belonging to projects the server no longer
// assigns to this node.
//
// It must only be called with a projects slice that came from a *successful*
// ListProjectsForReconcile. An error from that call means the agent does not
// know what it owns, and an unknown assignment must never be read as "every
// local project is an orphan" — hence the caller returns early rather than
// passing an empty slice here.
//
// projects is the same slice that drives reconciliation, not a second query:
// re-querying would open a window in which the project could be reassigned
// between the two reads, making the sweep act on a view the reconcile never saw.
func sweepOrphans(
	ctx context.Context,
	runtime docker.Runtime,
	routes RouteUpdater,
	tracker *orphanTracker,
	projects []*v1.Project,
	logger *zap.Logger,
) {
	localNames, err := runtime.ListLocalProjects(ctx)
	if err != nil {
		logger.Warn("Orphan sweep: failed to list local projects", zap.Error(err))
		return
	}
	if len(localNames) == 0 {
		return
	}

	assigned := make(map[string]struct{}, len(projects))
	for _, p := range projects {
		assigned[p.Name] = struct{}{}
	}

	for _, name := range tracker.observe(localNames, assigned) {
		log := logger.With(zap.String("project", name))
		log.Info("Removing orphaned project: absent from server assignment beyond grace period",
			zap.Duration("gracePeriod", orphanGracePeriod))

		if err := runtime.RemoveOrphanProject(ctx, name); err != nil {
			// Keep the candidate so the next tick retries; the grace period has
			// already elapsed, so the retry acts immediately.
			log.Error("Failed to remove orphaned project", zap.Error(err))
			continue
		}

		if routes != nil {
			routes.Remove(name)
			log.Info("Removed proxy routes for orphaned project")
		}

		tracker.forget(name)
	}
}
