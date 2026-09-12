package agent

import (
	"sync"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// Tier-1 recovery bounds. These are fixed for 1.0; no manifest field exposes
// them until real operational measurements justify a policy surface.
const (
	// maxRecoveryAttempts is how many times the agent restarts a Project's
	// containers before giving up and reporting LocalRestartExhausted.
	maxRecoveryAttempts = 3

	// recoveryVerifyTimeout is how long after the final attempt the agent waits
	// for the containers to come up before declaring exhaustion. Earlier
	// attempts do not need it: an attempt that has not produced a healthy
	// Project by the time its backoff expires has already failed, and the next
	// attempt supersedes it. Like the backoffs it is a minimum, evaluated on
	// the next poll after it expires.
	recoveryVerifyTimeout = 30 * time.Second

	// recoveryStableWindow is how long a Project must stay healthy before its
	// attempt count is forgotten. Without it a container that fails every
	// eleven minutes would be recovered forever, one attempt at a time, and
	// never look like the persistent fault it is.
	recoveryStableWindow = 10 * time.Minute

	// transientObservationTimeout bounds how long a Project may sit with a
	// container in Docker's "restarting" state before it is reported Failed.
	// cara sets no restart policy, so "restarting" only appears while
	// something is running `docker restart` and should settle within a poll or
	// two. One that does not settle is stuck, and without this bound the
	// Project would read Running indefinitely while it serves nothing.
	transientObservationTimeout = 30 * time.Second
)

// recoveryBackoff is the minimum delay before attempts 2 and 3, indexed by
// attempts already made minus one. There is no entry after the final attempt:
// what follows it is recoveryVerifyTimeout, which is a wait for the result,
// not a delay before another try.
//
// Growing delays give a dependency that is itself restarting — a database a
// service needs — time to come back, without waiting so long that a transient
// fault keeps a Project down.
//
// These are minimums, not a schedule. Recovery is evaluated only when the
// agent polls, every defaultPollInterval, so an attempt runs on the first
// poll after its backoff has expired: with the 10s poll, 5s and 10s both
// take effect roughly 10s after the attempt before them.
var recoveryBackoff = [maxRecoveryAttempts - 1]time.Duration{
	5 * time.Second,
	10 * time.Second,
}

// recoveryKey identifies one assignment of one Project.
//
// UID and generation are part of the identity, not decoration: a Project
// deleted and recreated under the same name is a different Project, and a
// Project reassigned away and back is a different grant of ownership. Either
// must start with a clean attempt count, or a new assignment would inherit the
// failures of an old one and give up immediately.
type recoveryKey struct {
	Namespace  string
	Name       string
	UID        string
	Generation int64
}

func keyForProject(p *v1.Project) recoveryKey {
	return recoveryKey{
		Namespace:  p.Namespace,
		Name:       p.Name,
		UID:        p.UID,
		Generation: p.Status.AssignmentGeneration,
	}
}

type recoveryEntry struct {
	attempts      int
	lastAttemptAt time.Time

	// healthySince is when the Project was first seen healthy after an
	// attempt. Zero while it is unhealthy.
	healthySince time.Time
}

// recoveryDecision is what the tracker says to do about an unhealthy Project
// on this poll.
type recoveryDecision int

const (
	// recoveryWait means do nothing: a backoff has not expired, or the final
	// attempt has not had its chance to take effect yet.
	recoveryWait recoveryDecision = iota

	// recoveryAttempt means perform the recovery action now. The attempt is
	// counted at this point, before the action runs, so an action that
	// succeeds and then crashes still consumes one.
	recoveryAttempt

	// recoveryExhausted means every attempt has been used and the Project
	// should be reported Failed with LocalRestartExhausted.
	recoveryExhausted
)

// recoveryTracker holds tier-1 recovery state for the Projects on this agent.
//
// The state is deliberately in memory. It is a retry counter for an operation
// measured in seconds, not a fact the control plane needs; durable recovery
// state belongs to the server-side tier-2 work. An agent restart therefore
// resets the counters, which is the safe direction — it retries a Project it
// might otherwise have abandoned.
type recoveryTracker struct {
	mu      sync.Mutex
	clock   clock
	entries map[recoveryKey]*recoveryEntry

	// transientSince is when a Project was first seen with a container
	// restarting, for as long as one still is. It is kept apart from entries
	// because it measures something else: not recovery attempts, but how
	// long judgement has been deferred.
	transientSince map[recoveryKey]time.Time
}

func newRecoveryTracker() *recoveryTracker {
	return &recoveryTracker{
		clock:          realClock{},
		entries:        make(map[recoveryKey]*recoveryEntry),
		transientSince: make(map[recoveryKey]time.Time),
	}
}

// next records that the Project is unhealthy and returns what to do about it.
//
// Calling it is what advances the state machine, so it must only be called
// once the caller has decided the Project is genuinely in need of recovery —
// not while a backup owns it, and not while Docker is mid-action.
func (t *recoveryTracker) next(key recoveryKey) recoveryDecision {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := t.entryLocked(key)

	// Any unhealthy observation ends the stable window: a Project that fails
	// again has not proven itself, whatever it did in between.
	e.healthySince = time.Time{}

	if e.attempts == 0 {
		e.attempts = 1
		e.lastAttemptAt = t.clock.Now()
		return recoveryAttempt
	}

	elapsed := t.clock.Since(e.lastAttemptAt)

	if e.attempts >= maxRecoveryAttempts {
		if elapsed >= recoveryVerifyTimeout {
			return recoveryExhausted
		}
		return recoveryWait
	}

	if elapsed < recoveryBackoff[e.attempts-1] {
		return recoveryWait
	}

	e.attempts++
	e.lastAttemptAt = t.clock.Now()
	return recoveryAttempt
}

// observeHealthy records that every container is running. It clears the
// Project's state once it has stayed healthy for the full stable window, so a
// Project that recovers and holds starts its next incident from zero.
func (t *recoveryTracker) observeHealthy(key recoveryKey) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		return
	}

	if e.healthySince.IsZero() {
		e.healthySince = t.clock.Now()
		return
	}

	if t.clock.Since(e.healthySince) >= recoveryStableWindow {
		delete(t.entries, key)
	}
}

// observeTransient records that the Project has a container restarting, and
// reports whether it has now been that way for longer than
// transientObservationTimeout.
//
// The first observation starts the clock. The key includes UID and
// generation, so a new assignment of the same Project starts a clock of its
// own rather than inheriting one that was nearly expired.
func (t *recoveryTracker) observeTransient(key recoveryKey) (stuck bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	since, ok := t.transientSince[key]
	if !ok {
		t.transientSince[key] = t.clock.Now()
		return false
	}
	return t.clock.Since(since) >= transientObservationTimeout
}

// clearTransient forgets that the Project was restarting. Called whenever it
// is seen with nothing restarting, whatever else it looks like, so the next
// restart is timed from its own start.
func (t *recoveryTracker) clearTransient(key recoveryKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.transientSince, key)
}

// attempts reports how many recovery attempts have been made for the key. It
// exists for logging and tests; the decision itself belongs to next.
func (t *recoveryTracker) attempts(key recoveryKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	if e, ok := t.entries[key]; ok {
		return e.attempts
	}
	return 0
}

// retain drops state for every Project that is no longer this agent's, keyed
// by the full identity. A reassignment or a delete-and-recreate produces a new
// key, so without this the old entry would sit in the map for the lifetime of
// the process.
//
// It is called with the complete set of Projects assigned to this node, so an
// empty set is meaningful: it clears everything. Callers must not invoke it
// when ownership is unknown.
func (t *recoveryTracker) retain(keys map[recoveryKey]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for k := range t.entries {
		if _, ok := keys[k]; !ok {
			delete(t.entries, k)
		}
	}
	for k := range t.transientSince {
		if _, ok := keys[k]; !ok {
			delete(t.transientSince, k)
		}
	}
}

func (t *recoveryTracker) entryLocked(key recoveryKey) *recoveryEntry {
	e, ok := t.entries[key]
	if !ok {
		e = &recoveryEntry{}
		t.entries[key] = e
	}
	return e
}
