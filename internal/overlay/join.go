package overlay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Default retry policy for transient join failures.
const (
	defaultMaxAttempts = 3
	defaultBackoff     = 5 * time.Second
)

// JoinWithRetry drives client.Join with a bounded retry policy that
// distinguishes permanent from transient failures:
//
//   - A failure wrapping ErrConfig (bad key file, malformed key, invalid
//     hostname) is permanent; it is returned immediately without retrying.
//   - Any other failure is treated as transient (e.g. Headscale not yet
//     reachable) and retried up to maxAttempts times with a fixed backoff.
//
// The returned error wraps the last join error so callers can log it and fail
// fast.  ctx cancellation aborts the retry loop.
func JoinWithRetry(ctx context.Context, client Client, logger *zap.Logger) (JoinResult, error) {
	return joinWithRetry(ctx, client, defaultMaxAttempts, defaultBackoff, logger)
}

func joinWithRetry(ctx context.Context, client Client, maxAttempts int, backoff time.Duration, logger *zap.Logger) (JoinResult, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := client.Join(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err

		// Permanent errors cannot be fixed by retrying.
		if errors.Is(err, ErrConfig) {
			return JoinResult{}, fmt.Errorf("overlay join failed: %w", err)
		}

		if attempt < maxAttempts {
			logger.Warn("Overlay join attempt failed, retrying",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("backoff", backoff),
				zap.Error(err),
			)
			select {
			case <-ctx.Done():
				return JoinResult{}, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}

	return JoinResult{}, fmt.Errorf("overlay join failed after %d attempts: %w", maxAttempts, lastErr)
}
