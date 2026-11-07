package agent

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Pacing of the repo host's repair. A healthy repo host costs one inspect and
// one ping per interval.
const (
	repoHostCheckInterval = 30 * time.Second
	repoHostCheckTimeout  = 2 * time.Minute
	repoHostMaxBackoff    = 5 * time.Minute
)

// repoHostReconciler is what keeping the repo host up needs: rebuild it from
// its durable state, starting or recreating the container when it is gone and
// reattaching every authorized network. *pgbackrest.RepoHost satisfies it.
type repoHostReconciler interface {
	Reconcile(ctx context.Context) error
}

// superviseRepoHost keeps the repo host running for as long as ctx lives.
//
// Placement authorizes a Project once, and a Running Project is never placed
// again, so nothing else would notice a repo host that exited, or one the
// Agent failed to rebuild when it started: the database would keep serving
// while every archive-push and backup failed. Each check is bounded and runs
// outside the poll loop, so a slow Docker cannot hold the heartbeat; failures
// back off, doubling up to repoHostMaxBackoff.
func superviseRepoHost(ctx context.Context, repo repoHostReconciler, interval, timeout, maxBackoff time.Duration, logger *zap.Logger) {
	wait := interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		checkCtx, cancel := context.WithTimeout(ctx, timeout)
		err := repo.Reconcile(checkCtx)
		cancel()
		if err == nil {
			wait = interval
			continue
		}
		if ctx.Err() != nil {
			return
		}
		wait = min(wait*2, maxBackoff)
		logger.Warn("The pgBackRest repo host is not healthy; retrying",
			zap.Duration("retryIn", wait), zap.Error(err))
	}
}
