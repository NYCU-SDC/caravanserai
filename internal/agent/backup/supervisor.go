package backup

import (
	"context"
	"errors"
	"sync"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// Supervisor owns one backup goroutine per Project, started when a Project
// with a backup policy is seen Running on this node and cancelled when it
// leaves.
//
// Backups must not run inline in the agent's poll loop. That loop shares a
// single goroutine with the heartbeat, so a multi-minute backup would starve
// the heartbeat, the control plane would mark this node NotReady, and the
// rescheduler would move the very Project being backed up — the backup would
// cause the failure it exists to protect against.
type Supervisor struct {
	runner *Runner
	logger *zap.Logger

	mu      sync.Mutex
	entries map[ResourceKey]*supervisorEntry
}

type supervisorEntry struct {
	cancel   context.CancelFunc
	interval time.Duration

	// mu guards project, which Sync refreshes on every poll so a run always
	// archives the current spec rather than whatever was current when the
	// goroutine started.
	mu      sync.Mutex
	project *v1.Project
}

func (e *supervisorEntry) current() *v1.Project {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.project
}

func (e *supervisorEntry) update(p *v1.Project) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.project = p
}

// NewSupervisor creates a Supervisor driving runner.
func NewSupervisor(runner *Runner, logger *zap.Logger) *Supervisor {
	return &Supervisor{
		runner:  runner,
		logger:  logger,
		entries: make(map[ResourceKey]*supervisorEntry),
	}
}

// Sync reconciles the running backup goroutines against the Projects this
// node currently holds. It is called once per poll with the same list that
// feeds reconcile, and is idempotent: a Project already supervised at the
// same interval is left running rather than restarted, so repeated polls
// never spawn duplicates.
//
// Projects that disappear from the list — reassigned, deleted, drained — have
// their goroutine cancelled.
func (s *Supervisor) Sync(ctx context.Context, projects []*v1.Project) {
	desired := make(map[ResourceKey]*v1.Project, len(projects))
	for _, p := range projects {
		if !shouldSupervise(p) {
			continue
		}
		desired[ResourceKey{Namespace: p.Namespace, Name: p.Name}] = p
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop supervisors for Projects that are no longer ours.
	for key, entry := range s.entries {
		if _, ok := desired[key]; !ok {
			entry.cancel()
			delete(s.entries, key)
			s.logger.Info("Stopped backup supervisor", zap.String("project", key.String()))
		}
	}

	for key, project := range desired {
		interval, err := backupInterval(project)
		if err != nil {
			// Validation rejects this at apply time, so reaching here means a
			// Project predating validation or written directly to the store.
			s.logger.Warn("Ignoring project with unusable backup interval",
				zap.String("project", key.String()), zap.Error(err))
			continue
		}

		if entry, ok := s.entries[key]; ok {
			if entry.interval == interval {
				// Already supervised at the right cadence; just refresh the
				// spec the next run will use.
				entry.update(project)
				continue
			}
			// The interval changed, so the timer must be rebuilt.
			entry.cancel()
			delete(s.entries, key)
		}

		runCtx, cancel := context.WithCancel(ctx)
		entry := &supervisorEntry{cancel: cancel, interval: interval, project: project}
		s.entries[key] = entry

		go s.loop(runCtx, key, entry)
		s.logger.Info("Started backup supervisor",
			zap.String("project", key.String()), zap.Duration("interval", interval))
	}
}

// Stop cancels every supervisor. Used on agent shutdown.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		entry.cancel()
		delete(s.entries, key)
	}
}

// Count reports how many supervisors are running. Exposed for tests and
// diagnostics.
func (s *Supervisor) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// loop waits one interval between runs.
//
// It uses a timer reset after each run rather than a ticker so the interval
// is measured from the end of one backup to the start of the next. A ticker
// would buffer a tick that elapsed during a long run and fire it immediately
// afterwards; the ticket requires such a tick to be skipped, not queued.
func (s *Supervisor) loop(ctx context.Context, key ResourceKey, entry *supervisorEntry) {
	timer := time.NewTimer(entry.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			project := entry.current()
			if err := s.runner.Run(ctx, project); err != nil && !errors.Is(err, ErrNoManagedVolumes) {
				s.logger.Error("Backup failed",
					zap.String("project", key.String()), zap.Error(err))
			}
			timer.Reset(entry.interval)
		}
	}
}

// shouldSupervise reports whether a Project needs a backup goroutine: it must
// be Running on this node, declare a backup policy, and have at least one
// Managed volume to capture.
func shouldSupervise(p *v1.Project) bool {
	if p.Status.Phase != v1.ProjectPhaseRunning {
		return false
	}
	if p.Spec.Backup == nil {
		return false
	}
	return len(managedVolumes(p.Spec)) > 0
}

// backupInterval parses a Project's configured backup cadence.
func backupInterval(p *v1.Project) (time.Duration, error) {
	d, err := time.ParseDuration(p.Spec.Backup.Interval)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("backup interval must be positive")
	}
	return d, nil
}
