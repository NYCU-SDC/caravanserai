package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

const (
	// defaultWorkers is the number of parallel reconcile goroutines per
	// controller when none is specified.
	defaultWorkers = 1

	// defaultRequeueAfter is the delay before re-enqueuing a key when
	// Result.Requeue is true.
	defaultRequeueAfter = 10 * time.Second

	// defaultErrorBackoff is the delay applied after a failed Reconcile call
	// before the same key is retried.
	defaultErrorBackoff = 5 * time.Second
)

// registration bundles a Controller with its runtime configuration.
type registration struct {
	ctrl    Controller
	workers int
}

// ManagerOption configures optional behaviour on the Manager.
type ManagerOption func(*managerOptions)

type managerOptions struct {
	requeueAfter time.Duration
	errorBackoff time.Duration
}

// WithRequeueAfter overrides the default requeue delay used when
// Result.Requeue is true.  This is intended for integration tests that need
// faster convergence.
func WithRequeueAfter(d time.Duration) ManagerOption {
	return func(o *managerOptions) {
		o.requeueAfter = d
	}
}

// WithErrorBackoff overrides the default error backoff delay used when
// Reconcile returns an error.  This is intended for integration tests.
func WithErrorBackoff(d time.Duration) ManagerOption {
	return func(o *managerOptions) {
		o.errorBackoff = d
	}
}

func applyManagerOptions(opts []ManagerOption) managerOptions {
	o := managerOptions{
		requeueAfter: defaultRequeueAfter,
		errorBackoff: defaultErrorBackoff,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Manager runs one or more Controllers in their own goroutine pools.
// It is the entry point for the control-plane loop.
//
// Usage:
//
//	mgr := controller.NewManager(logger)
//	mgr.Add(myCtrl)           // or mgr.AddWithWorkers(myCtrl, 3)
//	if err := mgr.Start(ctx); err != nil { ... }
type Manager struct {
	logger        *zap.Logger
	registrations []registration
	requeueAfter  time.Duration
	errorBackoff  time.Duration
}

// NewManager creates an empty Manager.  Register controllers with Add or
// AddWithWorkers before calling Start.
func NewManager(logger *zap.Logger, opts ...ManagerOption) *Manager {
	o := applyManagerOptions(opts)
	return &Manager{
		logger:       logger,
		requeueAfter: o.requeueAfter,
		errorBackoff: o.errorBackoff,
	}
}

// Add registers a controller with the default number of worker goroutines.
func (m *Manager) Add(ctrl Controller) {
	m.AddWithWorkers(ctrl, defaultWorkers)
}

// AddWithWorkers registers a controller with an explicit worker count.
// workers must be >= 1.
func (m *Manager) AddWithWorkers(ctrl Controller, workers int) {
	if workers < 1 {
		workers = defaultWorkers
	}
	m.registrations = append(m.registrations, registration{ctrl: ctrl, workers: workers})
}

// Start launches all registered controllers and blocks until ctx is cancelled.
// Each controller gets its own buffered work queue and a pool of worker
// goroutines.  Start returns the first non-nil error returned by any worker,
// or ctx.Err() when the context is cancelled normally.
func (m *Manager) Start(ctx context.Context) error {
	if len(m.registrations) == 0 {
		m.logger.Warn("Controller Manager started with no controllers registered")
	}

	// errCh receives the first fatal error from any goroutine.
	errCh := make(chan error, 1)

	for _, reg := range m.registrations {
		reg := reg // capture loop variable
		wq := newWorkQueue()

		m.logger.Info("Starting controller",
			zap.String("controller", reg.ctrl.Name()),
			zap.Int("workers", reg.workers),
		)

		// Seed the queue so the controller runs at least once on startup.
		go m.seed(ctx, reg.ctrl, wq)

		// Worker pool.
		for range reg.workers {
			go m.runWorker(ctx, reg.ctrl, wq, errCh)
		}
	}

	select {
	case <-ctx.Done():
		m.logger.Info("Controller Manager shutting down", zap.Error(ctx.Err()))
		return ctx.Err()
	case err := <-errCh:
		return fmt.Errorf("controller worker exited with fatal error: %w", err)
	}
}

// seed is the reconcile-trigger side of the control loop.  If the controller
// implements the optional Seeder interface, seed delegates to it so the
// controller can drive its own scheduling (e.g. periodic list + enqueue).
// Otherwise it logs once and returns, leaving the queue to be driven by
// external callers (e.g. HTTP handlers via Enqueue).
func (m *Manager) seed(ctx context.Context, ctrl Controller, wq *workQueue) {
	seeder, ok := ctrl.(Seeder)
	if !ok {
		m.logger.Debug("Controller has no Seeder, skipping seed goroutine",
			zap.String("controller", ctrl.Name()),
		)
		return
	}

	m.logger.Debug("Controller seed goroutine started",
		zap.String("controller", ctrl.Name()),
	)

	seeder.Seed(ctx, func(name string) {
		wq.Enqueue(name)
	})
}

// runWorker pulls items from the work queue and calls ctrl.Reconcile until ctx
// is done.
func (m *Manager) runWorker(ctx context.Context, ctrl Controller, wq *workQueue, errCh chan<- error) {
	log := m.logger.With(zap.String("controller", ctrl.Name()))
	log.Debug("Worker started")

	for {
		name, ok := wq.Get(ctx)
		if !ok {
			log.Debug("Worker stopping", zap.Error(ctx.Err()))
			return
		}

		result, err := ctrl.Reconcile(ctx, name)

		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Context cancelled mid-reconcile — not a fatal error.
			log.Debug("Reconcile interrupted by context", zap.String("name", name))
			return

		case err != nil:
			log.Error("Reconcile failed, will retry",
				zap.String("name", name),
				zap.Duration("backoff", m.errorBackoff),
				zap.Error(err),
			)
			backoff := m.errorBackoff
			go func() {
				select {
				case <-time.After(backoff):
					wq.Enqueue(name)
				case <-ctx.Done():
				}
			}()

		case result.Requeue:
			log.Debug("Reconcile requested requeue",
				zap.String("name", name),
				zap.Duration("after", m.requeueAfter),
			)
			requeueAfter := m.requeueAfter
			go func() {
				select {
				case <-time.After(requeueAfter):
					wq.Enqueue(name)
				case <-ctx.Done():
				}
			}()

		default:
			log.Debug("Reconcile complete", zap.String("name", name))
		}
	}
}
