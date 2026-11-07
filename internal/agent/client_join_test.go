package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestClient_JoinTradesTheJoinTokenForANodeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/nodes/node-1/join", r.URL.Path)
		assert.Equal(t, "Bearer cara_join", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(v1.NodeJoined{Token: "cara_node"})
	}))
	defer server.Close()

	token, err := NewClient(zap.NewNop(), server.URL, "node-1").Join(context.Background(), "cara_join")
	require.NoError(t, err)
	assert.Equal(t, "cara_node", token)
}

// A 4xx means the token itself is bad, so retrying cannot help; anything else
// may be the server not being up yet.
func TestClient_JoinTellsRefusalsFromFailures(t *testing.T) {
	for status, refused := range map[int]bool{
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           true,
		http.StatusBadRequest:          true,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		_, err := NewClient(zap.NewNop(), server.URL, "node-1").Join(context.Background(), "cara_join")
		server.Close()
		require.Error(t, err, "status %d", status)
		assert.Equal(t, refused, errorsIsJoinRefused(err), "status %d: %v", status, err)
	}

	_, err := NewClient(zap.NewNop(), "http://127.0.0.1:1", "node-1").Join(context.Background(), "cara_join")
	require.Error(t, err)
	assert.False(t, errorsIsJoinRefused(err), "an unreachable server is retried")
}

// A revoked token makes every request fail; the reason is logged once, not on
// every heartbeat, and its recovery once.
func TestClient_LogsARefusedTokenOnce(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusUnauthorized)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer cara_node", r.Header.Get("Authorization"))
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	core, logs := observer.New(zap.InfoLevel)
	c := NewClient(zap.New(core), server.URL, "node-1")
	c.SetToken("cara_node")
	for range 3 {
		_ = c.Heartbeat(context.Background(), v1.NodeStatus{})
	}
	assert.Equal(t, 1, logs.FilterMessageSnippet("refused this Node's token").Len())

	status.Store(http.StatusOK)
	_ = c.Heartbeat(context.Background(), v1.NodeStatus{})
	assert.Equal(t, 1, logs.FilterMessageSnippet("accepts this Node's token again").Len())
}

// Every 403 is logged with the request it refused, and the call still fails
// the ordinary way so its caller keeps treating it as transient (CARA-140).
func TestClient_LogsEachForbiddenRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	core, logs := observer.New(zap.InfoLevel)
	c := NewClient(zap.New(core), server.URL, "node-1")
	c.SetToken("cara_node")
	for range 2 {
		_, err := c.GetSecret(context.Background(), "s")
		assert.Error(t, err)
	}
	forbidden := logs.FilterMessageSnippet("forbade a request")
	assert.Equal(t, 2, forbidden.Len())
	assert.Equal(t, "/api/v1/secrets/s", forbidden.All()[0].ContextMap()["path"])
	assert.Equal(t, 0, logs.FilterMessageSnippet("refused this Node's token").Len(), "a 403 is not a revoked token")
}

func errorsIsJoinRefused(err error) bool { return errors.Is(err, ErrJoinRefused) }
