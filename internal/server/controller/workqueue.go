package controller

import (
	"context"
	"sync"
)

// workQueue is a deduplicating work queue for reconcile keys.  It replaces the
// plain chan-based queue to prevent the same name from being reconciled multiple
// times in quick succession when enqueued concurrently by seeds, event-bus
// subscribers, requeue goroutines, and HTTP handlers.
//
// The design is a simplified version of the dirty-set pattern used by
// client-go/util/workqueue in Kubernetes.
type workQueue struct {
	mu     sync.Mutex
	dirty  map[string]bool // names waiting to be processed
	notify chan struct{}   // len-1 buffered signal channel
}

// newWorkQueue creates a ready-to-use workQueue.
func newWorkQueue() *workQueue {
	return &workQueue{
		dirty:  make(map[string]bool),
		notify: make(chan struct{}, 1),
	}
}

// Enqueue adds name to the dirty set.  If the name is already present (i.e.
// waiting to be processed), the call is a no-op — this is the deduplication
// guarantee.  Enqueue is non-blocking and safe to call from any goroutine.
func (wq *workQueue) Enqueue(name string) {
	wq.mu.Lock()
	defer wq.mu.Unlock()

	if wq.dirty[name] {
		return // already queued — deduplicate
	}
	wq.dirty[name] = true

	// Non-blocking signal: if the channel already has a pending signal the
	// consumer will drain it and re-check the dirty set.
	select {
	case wq.notify <- struct{}{}:
	default:
	}
}

// Get blocks until a name is available or ctx is cancelled.  It returns the
// name and true, or ("", false) when the context is done.
//
// After popping one key, if the dirty set still has entries a new signal is
// sent on the notify channel so the next Get (or the same goroutine in a loop)
// wakes up promptly.
func (wq *workQueue) Get(ctx context.Context) (string, bool) {
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-wq.notify:
			wq.mu.Lock()
			// Pop an arbitrary key from the dirty set.
			var name string
			for name = range wq.dirty {
				break
			}
			if name == "" {
				// Spurious wake-up: dirty set was drained between signal
				// and lock acquisition.
				wq.mu.Unlock()
				continue
			}
			delete(wq.dirty, name)

			// Re-signal if there are more items so other workers (or the
			// next iteration) can pick them up.
			if len(wq.dirty) > 0 {
				select {
				case wq.notify <- struct{}{}:
				default:
				}
			}
			wq.mu.Unlock()
			return name, true
		}
	}
}

// Len returns the number of names currently waiting in the dirty set.
func (wq *workQueue) Len() int {
	wq.mu.Lock()
	defer wq.mu.Unlock()
	return len(wq.dirty)
}
