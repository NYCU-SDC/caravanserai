package controller

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkQueue_DeduplicatesBeforeGet enqueues the same name N times before
// any Get call and verifies the name is returned exactly once.
func TestWorkQueue_DeduplicatesBeforeGet(t *testing.T) {
	t.Helper()

	wq := newWorkQueue()

	const name = "my-node"
	const N = 10

	for range N {
		wq.Enqueue(name)
	}

	// The dirty set should contain exactly one entry.
	require.Equal(t, 1, wq.Len(), "dirty set should have exactly 1 entry after N duplicate enqueues")

	// Get should return the name exactly once.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, ok := wq.Get(ctx)
	require.True(t, ok, "Get should succeed")
	assert.Equal(t, name, got)

	// After the Get, the queue should be empty.
	assert.Equal(t, 0, wq.Len(), "dirty set should be empty after Get")

	// A second Get should block until context cancels (no more items).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()

	_, ok = wq.Get(ctx2)
	assert.False(t, ok, "second Get should return false (context cancelled, no items)")
}

// TestWorkQueue_DeduplicatesDuringRequeue simulates the scenario where a name
// is re-enqueued (e.g. by a requeue goroutine after a backoff sleep) while the
// same name is already in the dirty set.  The name should only be returned once
// by Get.
func TestWorkQueue_DeduplicatesDuringRequeue(t *testing.T) {
	t.Helper()

	wq := newWorkQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const name = "my-project"

	// Simulate: seed enqueues the name.
	wq.Enqueue(name)

	// Simulate: a requeue goroutine fires and tries to enqueue the same name
	// after a short delay, while the original is still in the dirty set.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		wq.Enqueue(name) // should be a no-op (deduplicated)
	}()

	// Wait for the requeue goroutine to run.
	wg.Wait()

	// The dirty set should still have exactly one entry.
	require.Equal(t, 1, wq.Len(), "dirty set should have 1 entry after dedup requeue")

	// Get returns the name once.
	got, ok := wq.Get(ctx)
	require.True(t, ok)
	assert.Equal(t, name, got)
	assert.Equal(t, 0, wq.Len())

	// No more items.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	_, ok = wq.Get(ctx2)
	assert.False(t, ok, "no more items expected")
}

// TestWorkQueue_GetBlocksUntilEnqueue verifies that Get blocks when the queue
// is empty and returns promptly once a name is enqueued.
func TestWorkQueue_GetBlocksUntilEnqueue(t *testing.T) {
	t.Helper()

	wq := newWorkQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const name = "delayed-item"

	// Enqueue after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		wq.Enqueue(name)
	}()

	got, ok := wq.Get(ctx)
	require.True(t, ok, "Get should succeed after enqueue")
	assert.Equal(t, name, got)
}

// TestWorkQueue_GetContextCancelled verifies that Get returns ("", false) when
// the context is cancelled while waiting.
func TestWorkQueue_GetContextCancelled(t *testing.T) {
	t.Helper()

	wq := newWorkQueue()
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately.
	cancel()

	got, ok := wq.Get(ctx)
	assert.False(t, ok, "Get should return false on cancelled context")
	assert.Empty(t, got)
}

// TestWorkQueue_MultipleNames verifies that multiple distinct names are all
// returned by Get (order is non-deterministic since the dirty set is a map).
func TestWorkQueue_MultipleNames(t *testing.T) {
	t.Helper()

	wq := newWorkQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	names := []string{"alpha", "beta", "gamma"}
	for _, n := range names {
		wq.Enqueue(n)
	}

	require.Equal(t, 3, wq.Len())

	var got []string
	for range names {
		name, ok := wq.Get(ctx)
		require.True(t, ok)
		got = append(got, name)
	}

	sort.Strings(got)
	sort.Strings(names)
	assert.Equal(t, names, got, "all distinct names should be returned exactly once")
	assert.Equal(t, 0, wq.Len())
}
