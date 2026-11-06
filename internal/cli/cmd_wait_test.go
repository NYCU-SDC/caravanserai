package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sequence serves the i-th response on the i-th request, then keeps serving
// the last. A response is either a status code (with a problem body) or a
// value to encode as JSON.
func sequence(t *testing.T, responses ...any) (*Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		switch resp := responses[min(i, len(responses)-1)].(type) {
		case int:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(resp)
			_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(resp), "status": resp, "detail": "from test"})
		default:
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), &n
}

func project(phase v1.ProjectPhase, conds ...v1.Condition) v1.Project {
	return v1.Project{ObjectMeta: v1.ObjectMeta{Name: "web"}, Status: v1.ProjectStatus{Phase: phase, Conditions: conds}}
}

func node(state v1.NodeState, heartbeat time.Time, conds ...v1.Condition) v1.Node {
	return v1.Node{ObjectMeta: v1.ObjectMeta{Name: "node-a"}, Status: v1.NodeStatus{State: state, LastHeartbeat: heartbeat, Conditions: conds}}
}

func runWait(t *testing.T, client *Client, kind, name, forFlag string, timeout time.Duration) (string, error) {
	t.Helper()
	cond, err := parseWaitFor(kind, forFlag)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var progress bytes.Buffer
	err = waitFor(ctx, client, kind, name, cond, &progress, time.Millisecond)
	return progress.String(), err
}

func TestWaitExitCodes(t *testing.T) {
	t.Run("met: 0", func(t *testing.T) {
		client, _ := sequence(t, project(v1.ProjectPhaseScheduled), project(v1.ProjectPhaseRunning))
		progress, err := runWait(t, client, "project", "web", "phase=Running", time.Second)
		require.NoError(t, err)
		assert.Contains(t, progress, "project/web Running")
	})

	t.Run("missing resource: 3, not a timeout", func(t *testing.T) {
		client, _ := sequence(t, http.StatusNotFound)
		_, err := runWait(t, client, "project", "web", "phase=Running", time.Second)
		require.Error(t, err)
		assert.Equal(t, ExitNotFound, ExitCode(err))
		assert.Contains(t, err.Error(), "project/web not found")
	})

	t.Run("timeout: 2, with the last state", func(t *testing.T) {
		blocked := v1.Condition{Type: v1.ConditionTypeSchedulingBlocked, Status: v1.ConditionTrue, Message: "node-b lacks cpu"}
		client, _ := sequence(t, project(v1.ProjectPhasePending, blocked))
		_, err := runWait(t, client, "project", "web", "phase=Running", 30*time.Millisecond)
		require.Error(t, err)
		assert.Equal(t, ExitTimeout, ExitCode(err))
		assert.Contains(t, err.Error(), "node-b lacks cpu")
	})

	t.Run("Failed while waiting for Running: 1", func(t *testing.T) {
		client, _ := sequence(t, project(v1.ProjectPhaseFailed, v1.Condition{
			Type: v1.ConditionTypePhase, Status: v1.ConditionTrue, Reason: "ReconcileError", Message: "pull image: no such host",
		}))
		_, err := runWait(t, client, "project", "web", "phase=Running", time.Second)
		require.Error(t, err)
		assert.Equal(t, 1, ExitCode(err))
		assert.Equal(t, `project "web" is Failed: ReconcileError: pull image: no such host`, err.Error())
	})

	t.Run("waiting for Failed itself succeeds", func(t *testing.T) {
		client, _ := sequence(t, project(v1.ProjectPhaseFailed))
		_, err := runWait(t, client, "project", "web", "phase=Failed", time.Second)
		require.NoError(t, err)
	})

	t.Run("refused request: 1 at once, not retried", func(t *testing.T) {
		client, n := sequence(t, http.StatusForbidden)
		_, err := runWait(t, client, "project", "web", "phase=Running", time.Second)
		require.Error(t, err)
		assert.Equal(t, 1, ExitCode(err))
		assert.EqualValues(t, 1, n.Load())
	})
}

func TestWaitRetriesAnUnavailableServer(t *testing.T) {
	t.Run("5xx is retried until the condition holds", func(t *testing.T) {
		client, _ := sequence(t, http.StatusServiceUnavailable, http.StatusBadGateway, project(v1.ProjectPhaseRunning))
		progress, err := runWait(t, client, "project", "web", "phase=Running", time.Second)
		require.NoError(t, err)
		assert.Contains(t, progress, "retrying")
	})

	t.Run("an unreachable server times out with the last error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := runWait(t, NewClient(url), "project", "web", "phase=Running", 30*time.Millisecond)
		require.Error(t, err)
		assert.Equal(t, ExitTimeout, ExitCode(err))
		assert.Contains(t, err.Error(), "last error")
	})
}

func TestWaitForDelete(t *testing.T) {
	t.Run("already gone: 0 on the first read", func(t *testing.T) {
		client, n := sequence(t, http.StatusNotFound)
		_, err := runWait(t, client, "project", "web", "delete", time.Second)
		require.NoError(t, err)
		assert.EqualValues(t, 1, n.Load())
	})

	t.Run("waits through Terminating", func(t *testing.T) {
		client, _ := sequence(t, project(v1.ProjectPhaseTerminating), project(v1.ProjectPhaseTerminated), http.StatusNotFound)
		progress, err := runWait(t, client, "project", "web", "delete", time.Second)
		require.NoError(t, err)
		assert.Contains(t, progress, "Terminating")
	})

	t.Run("a node", func(t *testing.T) {
		client, _ := sequence(t, node(v1.NodeStateReady, time.Time{}), http.StatusNotFound)
		_, err := runWait(t, client, "node", "node-a", "delete", time.Second)
		require.NoError(t, err)
	})
}

func TestWaitForCondition(t *testing.T) {
	ready := v1.Condition{Type: "Ready", Status: v1.ConditionTrue}
	notReady := v1.Condition{Type: "Ready", Status: v1.ConditionFalse}
	client, n := sequence(t, node(v1.NodeStateNotReady, time.Time{}, notReady), node(v1.NodeStateReady, time.Time{}, ready))
	_, err := runWait(t, client, "node", "node-a", "condition=Ready", time.Second)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n.Load(), "status False is not met")
}

func TestWaitForNodeState(t *testing.T) {
	client, _ := sequence(t, node(v1.NodeStateNotReady, time.Time{}), node(v1.NodeStateReady, time.Time{}))
	_, err := runWait(t, client, "node", "node-a", "phase=Ready", time.Second)
	require.NoError(t, err)
}

func TestWaitForHeartbeat(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

	t.Run("a heartbeat newer than the first read", func(t *testing.T) {
		client, n := sequence(t, node(v1.NodeStateReady, t0), node(v1.NodeStateReady, t0), node(v1.NodeStateReady, t0.Add(10*time.Second)))
		_, err := runWait(t, client, "node", "node-a", "heartbeat", time.Second)
		require.NoError(t, err)
		assert.EqualValues(t, 3, n.Load())
	})

	t.Run("the heartbeat it had at the start does not count", func(t *testing.T) {
		client, _ := sequence(t, node(v1.NodeStateReady, t0))
		_, err := runWait(t, client, "node", "node-a", "heartbeat", 30*time.Millisecond)
		assert.Equal(t, ExitTimeout, ExitCode(err))
	})

	t.Run("a node that never heartbeat, then does", func(t *testing.T) {
		client, _ := sequence(t, node(v1.NodeStateNotReady, time.Time{}), node(v1.NodeStateReady, t0))
		progress, err := runWait(t, client, "node", "node-a", "heartbeat", time.Second)
		require.NoError(t, err)
		assert.Contains(t, progress, "lastHeartbeat never")
	})
}

func TestParseWaitFor(t *testing.T) {
	for _, tc := range []struct{ kind, in, wantErr string }{
		{"project", "phase=Running", ""},
		{"project", "phase=running", "unknown project phase"},
		{"node", "phase=Ready", ""},
		{"node", "state=Draining", ""},
		{"node", "phase=Running", "unknown node state"},
		{"project", "heartbeat", "only to a node"},
		{"project", "condition=", "invalid --for"},
		{"project", "deleted", "invalid --for"},
		{"project", "delete=x", "invalid --for"},
	} {
		_, err := parseWaitFor(tc.kind, tc.in)
		if tc.wantErr == "" {
			assert.NoError(t, err, "%s %s", tc.kind, tc.in)
		} else {
			assert.ErrorContains(t, err, tc.wantErr, "%s %s", tc.kind, tc.in)
		}
	}
	_, err := waitKind("vm")
	assert.ErrorContains(t, err, "valid kinds are project, node")
}
