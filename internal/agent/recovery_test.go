package agent

import (
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestTracker drives the tracker from orphan_test.go's fakeClock, so the
// backoff and window boundaries are asserted exactly rather than slept through.
func newTestTracker() (*recoveryTracker, *fakeClock) {
	clk := &fakeClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
	return &recoveryTracker{
		clock:          clk,
		entries:        map[recoveryKey]*recoveryEntry{},
		transientSince: map[recoveryKey]time.Time{},
	}, clk
}

func testKey() recoveryKey {
	return recoveryKey{Namespace: "default", Name: "guestbook", UID: "uid-1", Generation: 7}
}

// The first failure is acted on immediately: waiting a backoff before the very
// first attempt would add latency to every recovery for no benefit.
func TestRecoveryActsOnTheFirstFailureImmediately(t *testing.T) {
	tr, _ := newTestTracker()

	assert.Equal(t, recoveryAttempt, tr.next(testKey()))
	assert.Equal(t, 1, tr.attempts(testKey()))
}

// An attempt is counted when the action starts, so an action that reports
// success and then crashes still consumes one. Without this a container that
// starts and immediately dies would be restarted forever.
func TestRecoveryBacksOffBetweenAttempts(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	require.Equal(t, recoveryAttempt, tr.next(key))

	// Still inside the 5s backoff for the second attempt.
	clk.advance(4 * time.Second)
	assert.Equal(t, recoveryWait, tr.next(key))
	assert.Equal(t, 1, tr.attempts(key), "waiting must not consume an attempt")

	clk.advance(2 * time.Second) // now 6s since attempt 1
	assert.Equal(t, recoveryAttempt, tr.next(key))
	assert.Equal(t, 2, tr.attempts(key))

	// Third attempt waits the longer 10s backoff.
	clk.advance(9 * time.Second)
	assert.Equal(t, recoveryWait, tr.next(key))
	clk.advance(2 * time.Second)
	assert.Equal(t, recoveryAttempt, tr.next(key))
	assert.Equal(t, 3, tr.attempts(key))
}

// After the final attempt the tracker waits the verification timeout before
// giving up, so a container that is slow to come up is not declared a failure
// while it is still starting.
func TestRecoveryExhaustsOnlyAfterTheVerificationTimeout(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	// Burn all three attempts.
	require.Equal(t, recoveryAttempt, tr.next(key))
	clk.advance(recoveryBackoff[0])
	require.Equal(t, recoveryAttempt, tr.next(key))
	clk.advance(recoveryBackoff[1])
	require.Equal(t, recoveryAttempt, tr.next(key))
	require.Equal(t, maxRecoveryAttempts, tr.attempts(key))

	clk.advance(recoveryVerifyTimeout - time.Second)
	assert.Equal(t, recoveryWait, tr.next(key), "still verifying the final attempt")

	clk.advance(2 * time.Second)
	assert.Equal(t, recoveryExhausted, tr.next(key))
}

// Exhaustion is stable: once reported it keeps reporting, rather than starting
// a fourth attempt or oscillating.
func TestRecoveryStaysExhausted(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	for i := 0; i < maxRecoveryAttempts; i++ {
		require.Equal(t, recoveryAttempt, tr.next(key))
		clk.advance(recoveryBackoff[i])
	}
	clk.advance(recoveryVerifyTimeout)

	assert.Equal(t, recoveryExhausted, tr.next(key))
	clk.advance(time.Minute)
	assert.Equal(t, recoveryExhausted, tr.next(key))
	assert.Equal(t, maxRecoveryAttempts, tr.attempts(key))
}

// Recovering is not enough; the Project has to hold. A container that fails
// every few minutes would otherwise be recovered forever, one attempt at a
// time, and never look like the persistent fault it is.
func TestRecoveryClearsStateOnlyAfterTheStableWindow(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	require.Equal(t, recoveryAttempt, tr.next(key))

	tr.observeHealthy(key) // starts the window
	clk.advance(recoveryStableWindow - time.Second)
	tr.observeHealthy(key)
	assert.Equal(t, 1, tr.attempts(key), "window not finished, attempts must survive")

	clk.advance(2 * time.Second)
	tr.observeHealthy(key)
	assert.Equal(t, 0, tr.attempts(key), "a full healthy window forgets the incident")
}

// Failing again mid-window restarts the clock, so a flapping Project cannot
// accumulate partial healthy periods into a reset.
func TestRecoveryFailureResetsTheStableWindow(t *testing.T) {
	tr, clk := newTestTracker()
	key := testKey()

	require.Equal(t, recoveryAttempt, tr.next(key))
	tr.observeHealthy(key)

	clk.advance(recoveryStableWindow - time.Second)
	clk.advance(recoveryBackoff[0])
	require.Equal(t, recoveryAttempt, tr.next(key), "failed again before the window closed")

	tr.observeHealthy(key)
	clk.advance(recoveryStableWindow - time.Second)
	tr.observeHealthy(key)
	assert.Equal(t, 2, tr.attempts(key), "the window restarted from the new failure")
}

// A Project reassigned away and back, or deleted and recreated under the same
// name, is a different grant of ownership and must not inherit the attempt
// count that made the previous one give up.
func TestRecoveryKeyDistinguishesGenerationAndUID(t *testing.T) {
	tr, _ := newTestTracker()

	gen7 := testKey()
	gen8 := testKey()
	gen8.Generation = 8
	recreated := testKey()
	recreated.UID = "uid-2"

	require.Equal(t, recoveryAttempt, tr.next(gen7))
	assert.Equal(t, 1, tr.attempts(gen7))
	assert.Equal(t, 0, tr.attempts(gen8), "a new generation starts clean")
	assert.Equal(t, 0, tr.attempts(recreated), "a new UID starts clean")
}

// retain is called with the complete ownership snapshot, so anything absent
// from it belongs to a Project this node no longer runs.
func TestRecoveryRetainDropsProjectsThatLeft(t *testing.T) {
	tr, _ := newTestTracker()

	stays := testKey()
	goes := testKey()
	goes.Name = "other"

	require.Equal(t, recoveryAttempt, tr.next(stays))
	require.Equal(t, recoveryAttempt, tr.next(goes))

	tr.retain(map[recoveryKey]struct{}{stays: {}})

	assert.Equal(t, 1, tr.attempts(stays))
	assert.Equal(t, 0, tr.attempts(goes))
}

func TestKeyForProjectUsesTheFullIdentity(t *testing.T) {
	p := &v1.Project{}
	p.Namespace = "default"
	p.Name = "guestbook"
	p.UID = "uid-1"
	p.Status.AssignmentGeneration = 7

	assert.Equal(t, testKey(), keyForProject(p))
}

// Only faults with a safe narrow operation are recovered. Paused and
// unrecognised states are reported for a human instead, and must not consume
// an attempt on the way.
func TestRecoverableExcludesPausedAndUnknown(t *testing.T) {
	assert.True(t, recoverable([]serviceState{{Health: healthStartable}}))
	assert.True(t, recoverable([]serviceState{{Health: healthNeedsRecreate}}))

	assert.False(t, recoverable([]serviceState{{Health: healthPaused}}))
	assert.False(t, recoverable([]serviceState{{Health: healthUnknown}}))
	assert.False(t, recoverable([]serviceState{
		{Health: healthStartable},
		{Health: healthPaused},
	}), "one unrecoverable fault makes the whole Project unrecoverable")
	assert.False(t, recoverable(nil))
}

// A recovery attempt names exactly the services that are broken, so the
// runtime has no licence to touch a healthy one.
func TestServiceNamesListsOnlyTheFaultyServices(t *testing.T) {
	bad := []serviceState{
		{Service: "web", Health: healthStartable},
		{Service: "db", Health: healthNeedsRecreate},
	}

	assert.Equal(t, []string{"web", "db"}, serviceNames(bad))
}
