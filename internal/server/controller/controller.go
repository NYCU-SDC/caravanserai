package controller

import "context"

// Result is returned by Reconcile to tell the Manager what to do next.
type Result struct {
	// Requeue instructs the Manager to requeue the same key after RequeueAfter
	// has elapsed (or immediately if RequeueAfter is zero).
	Requeue bool
}

// Controller is the interface every domain controller must satisfy.
//
// The design intentionally mirrors controller-runtime's Reconciler: each
// controller owns exactly one resource kind and receives the name of the
// object that needs attention.  The Manager calls Reconcile whenever:
//
//   - an object of the watched kind is created or updated
//   - the Manager decides it is time to re-evaluate the object (e.g. after
//     a requeueing interval)
//
// Implementations must be safe for concurrent calls with different keys, but
// the Manager guarantees that the same key is never being reconciled twice at
// the same time.
type Controller interface {
	// Name returns a stable, human-readable identifier used in log messages
	// and metrics (e.g. "node-health", "project-scheduler").
	Name() string

	// Reconcile drives the object identified by name towards its desired
	// state.  It must be idempotent: calling it multiple times with the same
	// key must produce the same outcome.
	//
	// Returning a non-nil error causes the Manager to requeue the key with
	// exponential back-off.  Return Result{Requeue: true} to schedule an
	// explicit retry without treating the situation as an error.
	Reconcile(ctx context.Context, name string) (Result, error)
}

// Seeder is an optional interface that Controllers may implement to drive their
// own reconcile scheduling.  The Manager calls Seed in a dedicated goroutine
// after startup; the controller should continuously watch for objects that need
// attention and call enqueue for each one.
//
// Seed must respect ctx cancellation and return promptly when ctx is done.
// The enqueue callback is safe to call from any goroutine.
type Seeder interface {
	Seed(ctx context.Context, enqueue func(name string))
}
