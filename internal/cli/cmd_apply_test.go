package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectSequence serves each status in turn, then keeps serving the last.
func projectSequence(t *testing.T, statuses ...v1.ProjectStatus) *Client {
	t.Helper()
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := statuses[min(i, len(statuses)-1)]
		i++
		_ = json.NewEncoder(w).Encode(v1.Project{ObjectMeta: v1.ObjectMeta{Name: "web"}, Status: s})
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestWaitForProject(t *testing.T) {
	blocked := v1.ProjectStatus{Phase: v1.ProjectPhasePending, Conditions: []v1.Condition{{
		Type: v1.ConditionTypeSchedulingBlocked, Status: v1.ConditionTrue, Reason: "NoSuitableNode", Message: "node-b lacks cpu",
	}}}

	t.Run("returns once Running and explains the wait", func(t *testing.T) {
		client := projectSequence(t, blocked, blocked, v1.ProjectStatus{Phase: v1.ProjectPhaseRunning})
		var progress bytes.Buffer
		require.NoError(t, waitForProject(context.Background(), client, "web", &progress, time.Millisecond))
		assert.Contains(t, progress.String(), "node-b lacks cpu")
		assert.Contains(t, progress.String(), "project/web Running")
		assert.Equal(t, 1, bytes.Count(progress.Bytes(), []byte("lacks cpu")), "an unchanged state is printed once")
	})

	t.Run("Failed is an error", func(t *testing.T) {
		client := projectSequence(t, v1.ProjectStatus{Phase: v1.ProjectPhaseFailed})
		var progress bytes.Buffer
		assert.ErrorContains(t, waitForProject(context.Background(), client, "web", &progress, time.Millisecond), "Failed")
	})

	t.Run("a timeout reports the last reason", func(t *testing.T) {
		client := projectSequence(t, blocked)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		var progress bytes.Buffer
		err := waitForProject(ctx, client, "web", &progress, time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "node-b lacks cpu")
	})
}
