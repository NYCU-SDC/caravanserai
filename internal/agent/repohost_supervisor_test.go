package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// scriptedReconciler fails the first failures calls, then succeeds, recording
// when each call came.
type scriptedReconciler struct {
	mu       sync.Mutex
	failures int
	calls    []time.Time
	deadline []bool
}

func (s *scriptedReconciler) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, has := ctx.Deadline()
	s.calls = append(s.calls, time.Now())
	s.deadline = append(s.deadline, has)
	if len(s.calls) <= s.failures {
		return errors.New("repo host not ready")
	}
	return nil
}

func (s *scriptedReconciler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// The repo host is checked again and again, each check bounded; a failing one
// is retried after a growing pause, and a success restores the interval.
func TestRepoHostIsKeptUpWithBackoff(t *testing.T) {
	r := &scriptedReconciler{failures: 3}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseRepoHost(ctx, r, 10*time.Millisecond, time.Second, 80*time.Millisecond, zap.NewNop())
		close(done)
	}()
	require.Eventually(t, func() bool { return r.count() >= 7 }, 3*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, has := range r.deadline {
		assert.True(t, has, "every check is bounded")
	}
	gap := func(i int) time.Duration { return r.calls[i].Sub(r.calls[i-1]) }
	assert.GreaterOrEqual(t, gap(3), 70*time.Millisecond, "after three failures the pause has grown to the cap")
	assert.Less(t, gap(5), 60*time.Millisecond, "after a success the interval is back")
}

func TestRepoHostSupervisionEndsWithTheAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseRepoHost(ctx, &scriptedReconciler{}, time.Hour, time.Second, time.Hour, zap.NewNop())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervision outlived its context")
	}
}
