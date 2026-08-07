package backup

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorClaimIsExclusive(t *testing.T) {
	c := NewCoordinator()
	key := ResourceKey{Namespace: "default", Name: "blog"}

	release, ok := c.TryClaim(key, OpBackup)
	require.True(t, ok)
	require.NotNil(t, release)

	// A second operation on the same Project must lose, whatever it is.
	_, ok = c.TryClaim(key, OpTerminate)
	assert.False(t, ok, "terminate must not overlap a backup")
	_, ok = c.TryClaim(key, OpRestore)
	assert.False(t, ok, "restore must not overlap a backup")
	_, ok = c.TryClaim(key, OpGC)
	assert.False(t, ok, "GC must not overlap a backup")

	release()

	// Once released the Project can be claimed again.
	_, ok = c.TryClaim(key, OpTerminate)
	assert.True(t, ok)
}

func TestCoordinatorClaimsArePerProject(t *testing.T) {
	c := NewCoordinator()
	blog := ResourceKey{Namespace: "default", Name: "blog"}
	wiki := ResourceKey{Namespace: "default", Name: "wiki"}

	_, ok := c.TryClaim(blog, OpBackup)
	require.True(t, ok)

	// A different Project is unaffected — backups run concurrently across
	// Projects, only never twice on the same one.
	_, ok = c.TryClaim(wiki, OpBackup)
	assert.True(t, ok)
}

func TestCoordinatorNamespaceIsPartOfIdentity(t *testing.T) {
	c := NewCoordinator()
	a := ResourceKey{Namespace: "team-a", Name: "blog"}
	b := ResourceKey{Namespace: "team-b", Name: "blog"}

	_, ok := c.TryClaim(a, OpBackup)
	require.True(t, ok)

	_, ok = c.TryClaim(b, OpBackup)
	assert.True(t, ok, "same name in a different namespace is a different Project")
}

func TestCoordinatorReleaseIsIdempotent(t *testing.T) {
	c := NewCoordinator()
	key := ResourceKey{Namespace: "default", Name: "blog"}

	release, ok := c.TryClaim(key, OpBackup)
	require.True(t, ok)

	release()
	release() // A double defer or retry must not free someone else's claim.

	otherRelease, ok := c.TryClaim(key, OpRestore)
	require.True(t, ok)

	// The stale release from the first claim must not drop the second one.
	release()
	assert.True(t, c.IsBusy(key), "a stale release must not free a newer claim")

	otherRelease()
	assert.False(t, c.IsBusy(key))
}

func TestCoordinatorIsBusy(t *testing.T) {
	c := NewCoordinator()
	key := ResourceKey{Namespace: "default", Name: "blog"}

	assert.False(t, c.IsBusy(key))

	release, ok := c.TryClaim(key, OpBackup)
	require.True(t, ok)
	assert.True(t, c.IsBusy(key))

	release()
	assert.False(t, c.IsBusy(key))
}

func TestCoordinatorCurrent(t *testing.T) {
	c := NewCoordinator()
	key := ResourceKey{Namespace: "default", Name: "blog"}

	_, held := c.Current(key)
	assert.False(t, held)

	release, ok := c.TryClaim(key, OpFinalBackup)
	require.True(t, ok)

	op, held := c.Current(key)
	assert.True(t, held)
	assert.Equal(t, OpFinalBackup, op)

	release()
	_, held = c.Current(key)
	assert.False(t, held)
}

func TestCoordinatorConcurrentClaims(t *testing.T) {
	// Run with -race: exactly one of many concurrent claimants must win.
	c := NewCoordinator()
	key := ResourceKey{Namespace: "default", Name: "blog"}

	const goroutines = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, ok := c.TryClaim(key, OpBackup); ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, won, "exactly one claimant may hold a Project")
}

func TestShouldRestartContainers(t *testing.T) {
	tests := []struct {
		name      string
		reason    ExitReason
		ownership Ownership
		want      bool
	}{
		// Still owned: the service was running before the backup and must be
		// running after it, whatever went wrong.
		{"failure while still owned", ExitFailure, OwnershipRetained, true},
		{"timeout while still owned", ExitTimeout, OwnershipRetained, true},
		{"cancelled while still owned", ExitCancelled, OwnershipRetained, true},
		{"agent shutdown while still owned", ExitAgentShutdown, OwnershipRetained, true},
		{"success while still owned", ExitSuccess, OwnershipRetained, true},

		// Ownership gone: restarting would double-run the workload.
		{"reassigned after failure", ExitFailure, OwnershipReassigned, false},
		{"reassigned after success", ExitSuccess, OwnershipReassigned, false},
		{"terminating after failure", ExitFailure, OwnershipTerminating, false},
		{"terminating after success", ExitSuccess, OwnershipTerminating, false},
		{"assignment lost after failure", ExitFailure, OwnershipLost, false},
		{"assignment lost after success", ExitSuccess, OwnershipLost, false},

		// Unknown ownership: prefer keeping the service up. A control-plane
		// outage must not become a service outage; a genuinely reassigned
		// Project is cleaned up once polling resumes.
		{"unknown ownership after failure", ExitFailure, OwnershipUnknown, true},
		{"unknown ownership after success", ExitSuccess, OwnershipUnknown, true},
		{"unknown ownership on shutdown", ExitAgentShutdown, OwnershipUnknown, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ShouldRestartContainers(tt.reason, tt.ownership))
		})
	}
}

func TestShouldRestartContainersNeverRestartsWhenOwnershipGone(t *testing.T) {
	// Whatever the exit reason, a Project this node no longer owns must never
	// have its containers restarted — this is the split-brain guard.
	gone := []Ownership{OwnershipReassigned, OwnershipTerminating, OwnershipLost}
	reasons := []ExitReason{ExitSuccess, ExitFailure, ExitTimeout, ExitCancelled, ExitAgentShutdown}

	for _, o := range gone {
		for _, r := range reasons {
			assert.False(t, ShouldRestartContainers(r, o),
				"ownership=%s reason=%s must not restart", o, r)
		}
	}
}

func TestExitReasonAndOwnershipStrings(t *testing.T) {
	assert.Equal(t, "Success", ExitSuccess.String())
	assert.Equal(t, "Failure", ExitFailure.String())
	assert.Equal(t, "Timeout", ExitTimeout.String())
	assert.Equal(t, "Cancelled", ExitCancelled.String())
	assert.Equal(t, "AgentShutdown", ExitAgentShutdown.String())

	assert.Equal(t, "Retained", OwnershipRetained.String())
	assert.Equal(t, "Reassigned", OwnershipReassigned.String())
	assert.Equal(t, "Terminating", OwnershipTerminating.String())
	assert.Equal(t, "Lost", OwnershipLost.String())
	assert.Equal(t, "Unknown", OwnershipUnknown.String())
}
