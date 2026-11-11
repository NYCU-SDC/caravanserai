package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// In-place spec updates of a Running Project (ADR-0026, CARA-134).
//
// The poll loop asks, before it judges a Running Project's health, whether its
// containers were built from the current spec. If not, this tick is the
// update's, and the health check waits for the next one: a container the
// update removed and has not created yet must not be read as a crash, and the
// recreate must not count as a tier-1 restart.

const (
	// specUpdateRetryInterval is how long after a failed update attempt the
	// next one waits. A tag that does not exist would otherwise be pulled
	// every poll.
	specUpdateRetryInterval = time.Minute

	// maxSpecUpdateAttempts is how many attempts that fail after stopping a
	// container are made before the Project is reported Failed. A failed pull
	// stops nothing, leaves the old containers serving and is not counted.
	maxSpecUpdateAttempts = maxRecoveryAttempts

	// reasonSpecUpdateFailed is the SpecApplied reason, and the Phase reason
	// once attempts run out, for an update that failed after it had begun
	// replacing containers.
	reasonSpecUpdateFailed = "SpecUpdateFailed"
)

// specUpdateFailure is the record of failed attempts at one revision.
type specUpdateFailure struct {
	revision int64
	// stoppedFailures counts attempts that failed after stopping something.
	stoppedFailures int
	lastAttemptAt   time.Time
}

// specUpdateDue reports whether an update to revision may be attempted now.
func (t *recoveryTracker) specUpdateDue(key recoveryKey, revision int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.specUpdates[key]
	if !ok || f.revision != revision {
		return true
	}
	return t.clock.Since(f.lastAttemptAt) >= specUpdateRetryInterval
}

// specUpdateFailed records a failed attempt at revision and returns how many
// attempts at it have failed after stopping something.
func (t *recoveryTracker) specUpdateFailed(key recoveryKey, revision int64, stoppedSomething bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.specUpdates == nil {
		t.specUpdates = make(map[recoveryKey]*specUpdateFailure)
	}
	f, ok := t.specUpdates[key]
	if !ok || f.revision != revision {
		f = &specUpdateFailure{revision: revision}
		t.specUpdates[key] = f
	}
	f.lastAttemptAt = t.clock.Now()
	if stoppedSomething {
		f.stoppedFailures++
	}
	return f.stoppedFailures
}

// specUpdateSucceeded forgets the failures recorded for the key.
func (t *recoveryTracker) specUpdateSucceeded(key recoveryKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.specUpdates, key)
}

// specUpdateNeeded reports whether drift calls for an update. A missing
// container counts only while the latest revision is not yet reported
// running: then it is what an update interrupted part-way leaves behind.
// Once the revision is running, a missing container is a crash and belongs to
// tier-1 recovery.
func specUpdateNeeded(p *v1.Project, drift docker.SpecDrift) bool {
	if drift.Changes() {
		return true
	}
	return p.Status.ObservedSpecRevision < p.SpecRevision && len(drift.Missing) > 0
}

// applySpecUpdate brings a Running Project's containers to its current spec
// when they are not. took reports whether it used this tick; when it did not,
// the health check runs as before. upToDate reports that the containers were
// compared with the spec and none is outdated, missing or left over, which is
// what lets a healthy tick report the revision running.
//
//	SpecDrift on the polled copy   nothing to do              → health check
//	retry interval                 last attempt too recent    → skip
//	claim OpUpdate                 another operation holds it → skip
//	re-read the Project            server unreachable         → skip
//	fence: UID, generation,        anything differs           → skip
//	  nodeRef, Running
//	fresh Maintenance              active                     → skip
//	WorkloadStopped evidence       recorded (ADR-0017)        → skip
//	SpecDrift on the fresh copy    nothing to do              → health check
//	resolve Secrets                Secret or key absent       → SpecApplied False
//	UpdateServices                 pull failed                → SpecApplied False, old containers run
//	                               another assignment's       → RecoveryBlocked
//	                               other failure              → SpecApplied False; Failed after
//	                                                            maxSpecUpdateAttempts
//	report SpecApplied True, refresh proxy routes
func applySpecUpdate(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, busy busyChecker, recovery *recoveryTracker, p *v1.Project, log *zap.Logger) (took, upToDate bool) {
	if p.SpecRevision == 0 {
		// A server without spec revisions: nothing to compare with.
		return false, false
	}
	drift, err := runtime.SpecDrift(ctx, p)
	if err != nil {
		// Another assignment's container is the classification's to report;
		// anything else leaves the tick to the health check as well.
		if !errors.Is(err, docker.ErrContainerNotOwned) {
			log.Warn("Could not compare containers with the spec", zap.Error(err))
		}
		return false, false
	}
	if !specUpdateNeeded(p, drift) {
		return false, !drift.Changes() && len(drift.Missing) == 0
	}
	key := keyForProject(p)
	if recovery != nil && !recovery.specUpdateDue(key, p.SpecRevision) {
		log.Debug("Waiting before retrying the spec update", zap.Int64("specRevision", p.SpecRevision))
		return true, false
	}

	if busy != nil {
		release, ok := busy.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpUpdate)
		if !ok {
			log.Info("Another operation holds the Project, not updating this tick")
			return true, false
		}
		defer release()
	}

	current, err := client.GetProject(ctx, p.Name)
	if err != nil {
		log.Warn("Could not re-read the Project before updating, skipping this tick", zap.Error(err))
		return true, false
	}
	if err := checkRecoveryFence(p, current, client.NodeName()); err != nil {
		log.Info("Project changed before the update could run, skipping", zap.Error(err))
		return true, false
	}
	if v1.IsMaintenanceActive(current.Status.Conditions, time.Now()) {
		log.Info("Project entered maintenance before the update could run, skipping")
		return true, false
	}
	identity := v1.AssignmentIdentity{ProjectUID: current.UID, NodeName: current.Status.NodeRef, AssignmentGeneration: current.Status.AssignmentGeneration}
	if current.Status.Recovery.WorkloadStoppedFor(identity) {
		log.Info("This assignment reported its workload stopped; not starting containers for an update")
		return true, false
	}

	drift, err = runtime.SpecDrift(ctx, current)
	if err != nil {
		if !errors.Is(err, docker.ErrContainerNotOwned) {
			log.Warn("Could not compare containers with the spec", zap.Error(err))
		}
		return false, false
	}
	if !specUpdateNeeded(current, drift) {
		// The spec moved back while this tick waited; the polled copy is
		// stale, so nothing is reported from it.
		return false, false
	}

	revision := current.SpecRevision
	fence := fenceForProject(current)
	report := func(applied bool, reason, message string) {
		if err := client.ReportSpecApplied(ctx, current.Name, fence, revision, applied, reason, message); err != nil {
			log.Warn("Could not report the spec revision", zap.Int64("specRevision", revision), zap.Error(err))
		}
	}

	resolved, err := resolveSecrets(ctx, client, current)
	if err != nil {
		if reason, message, blocked := secretBlocked(err); blocked {
			log.Warn("Spec update blocked: a Secret it needs does not exist", zap.String("reason", reason), zap.Error(err))
			if recovery != nil {
				recovery.specUpdateFailed(key, revision, false)
			}
			report(false, reason, message)
			return true, false
		}
		log.Warn("Could not fetch Secrets for the update, will retry next poll", zap.Error(err))
		return true, false
	}

	log.Info("Updating containers to the new spec",
		zap.Int64("specRevision", revision),
		zap.Strings("outdated", drift.Outdated),
		zap.Strings("missing", drift.Missing),
		zap.Int("removed", len(drift.Removed)))

	if err := runtime.UpdateServices(ctx, resolved, drift); err != nil {
		switch {
		case errors.Is(err, docker.ErrImagePull):
			log.Warn("Spec update stopped before any container: an image could not be pulled", zap.Error(err))
			if recovery != nil {
				recovery.specUpdateFailed(key, revision, false)
			}
			report(false, v1.ReasonImagePullFailed, err.Error())
		case errors.Is(err, docker.ErrContainerNotOwned):
			log.Warn("Spec update aborted: a container became another assignment's", zap.Error(err))
			reportRecoveryBlocked(ctx, client, current, recoveryBlockedStaleContainer, staleContainerMessage(err), log)
		default:
			failures := 1
			if recovery != nil {
				failures = recovery.specUpdateFailed(key, revision, true)
			}
			msg := fmt.Sprintf("attempt %d of %d: %v", failures, maxSpecUpdateAttempts, err)
			log.Warn("Spec update failed", zap.String("detail", msg))
			report(false, reasonSpecUpdateFailed, msg)
			if failures >= maxSpecUpdateAttempts {
				exhausted := fmt.Sprintf("Spec revision %d could not be applied after %d attempts: %v", revision, failures, err)
				_ = client.UpdateProjectStatus(ctx, current.Name, fence, v1.ProjectPhaseFailed, reasonSpecUpdateFailed, exhausted)
			}
		}
		return true, false
	}

	if recovery != nil {
		recovery.specUpdateSucceeded(key)
	}
	log.Info("Containers run the new spec", zap.Int64("specRevision", revision))
	report(true, "", "")
	updateProxyRoutes(ctx, runtime, routes, current, log)
	return true, true
}

// reportSpecRunning tells the server that p's containers run its spec
// revision, when the server does not know it yet. Called only once every
// service is running and none is outdated.
func reportSpecRunning(ctx context.Context, client *Client, p *v1.Project, log *zap.Logger) {
	if p.SpecRevision == 0 || p.Status.ObservedSpecRevision >= p.SpecRevision {
		return
	}
	if err := client.ReportSpecApplied(ctx, p.Name, fenceForProject(p), p.SpecRevision, true, "", ""); err != nil {
		log.Warn("Could not report the spec revision", zap.Int64("specRevision", p.SpecRevision), zap.Error(err))
	}
}
