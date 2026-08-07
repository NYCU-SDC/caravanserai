package overlay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// joinStep is one scripted outcome of a Client.Join call.
type joinStep struct {
	res JoinResult
	err error
}

func ok(ip string) joinStep   { return joinStep{res: JoinResult{OverlayIP: ip}} }
func fail(err error) joinStep { return joinStep{err: err} }

// fakeClient is a Client that returns scripted results per Join call and
// records how many times Join was invoked.  Once the script is exhausted it
// keeps returning the last step, so an "always transient" client can be built
// from a single failing step.
type fakeClient struct {
	steps []joinStep
	calls int
}

func (f *fakeClient) Join(ctx context.Context) (JoinResult, error) {
	i := f.calls
	f.calls++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	return f.steps[i].res, f.steps[i].err
}

func (f *fakeClient) OverlayIP() string { return "" }
func (f *fakeClient) Close() error      { return nil }

func TestJoinWithRetry(t *testing.T) {
	transient := errors.New("headscale unreachable")
	configErr := fmt.Errorf("%w: bad key file", ErrConfig)

	tests := []struct {
		name        string
		steps       []joinStep
		maxAttempts int
		wantIP      string
		wantErr     bool
		wantCalls   int
	}{
		{
			name:        "succeeds on first attempt",
			steps:       []joinStep{ok("100.64.0.5")},
			maxAttempts: 3,
			wantIP:      "100.64.0.5",
			wantCalls:   1,
		},
		{
			name:        "retries transient failure then succeeds",
			steps:       []joinStep{fail(transient), ok("100.64.0.7")},
			maxAttempts: 3,
			wantIP:      "100.64.0.7",
			wantCalls:   2,
		},
		{
			name:        "config error is not retried",
			steps:       []joinStep{fail(configErr), ok("100.64.0.9")},
			maxAttempts: 3,
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "transient failure exhausts attempts",
			steps:       []joinStep{fail(transient)},
			maxAttempts: 3,
			wantErr:     true,
			wantCalls:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{steps: tt.steps}
			// Zero backoff keeps the test fast.
			res, err := joinWithRetry(context.Background(), fc, tt.maxAttempts, 0, zap.NewNop())

			assert.Equal(t, tt.wantCalls, fc.calls, "unexpected number of Join calls")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantIP, res.OverlayIP)
		})
	}
}

func TestJoinWithRetry_ContextCancelled(t *testing.T) {
	transient := errors.New("headscale unreachable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A long backoff guarantees the select falls through to ctx.Done() rather
	// than the backoff timer, so the retry loop aborts on cancellation.
	fc := &fakeClient{steps: []joinStep{fail(transient)}}
	_, err := joinWithRetry(ctx, fc, 3, time.Hour, zap.NewNop())

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, fc.calls)
}
