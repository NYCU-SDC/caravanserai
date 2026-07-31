package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestClient_OverlayIP(t *testing.T) {
	c := NewClient(zap.NewNop(), "http://localhost:8080", "node-1")

	// Empty until the agent joins the overlay.
	assert.Empty(t, c.OverlayIP())

	c.SetOverlayIP("100.64.0.5")
	assert.Equal(t, "100.64.0.5", c.OverlayIP())
}

func TestClient_RegisterReportsOverlayIP(t *testing.T) {
	var got v1.Node
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/nodes", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	c := NewClient(zap.NewNop(), server.URL, "node-1")
	c.SetOverlayIP("100.64.0.5")

	err := c.Register(context.Background(), v1.NodeSpec{Hostname: "node-1"})
	require.NoError(t, err)
	assert.Equal(t, "100.64.0.5", got.Status.Network.OverlayIP)
}

func TestClient_RegisterConflictRefreshesOverlayIP(t *testing.T) {
	var got heartbeatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			w.WriteHeader(http.StatusConflict)
		case "/api/v1/nodes/node-1/heartbeat":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := NewClient(zap.NewNop(), server.URL, "node-1")
	c.SetOverlayIP("100.64.0.6")

	err := c.Register(context.Background(), v1.NodeSpec{Hostname: "node-1"})
	require.NoError(t, err)
	assert.Equal(t, "100.64.0.6", got.Network.OverlayIP)
}

func TestClient_HeartbeatDefaultsOverlayIPFromClient(t *testing.T) {
	var got heartbeatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/nodes/node-1/heartbeat", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewClient(zap.NewNop(), server.URL, "node-1")
	c.SetOverlayIP("100.64.0.7")

	err := c.Heartbeat(context.Background(), v1.NodeStatus{State: v1.NodeStateReady})
	require.NoError(t, err)
	assert.Equal(t, "100.64.0.7", got.Network.OverlayIP)
}

func TestHeartbeatNetworkStatus(t *testing.T) {
	c := NewClient(zap.NewNop(), "http://localhost:8080", "node-1")

	status := heartbeatNetworkStatus(c, 9090, "192.0.2.10")
	assert.Equal(t, "192.0.2.10", status.OverlayIP)
	assert.Equal(t, 9090, status.AgentPort)

	c.SetOverlayIP("100.64.0.8")
	status = heartbeatNetworkStatus(c, 9091, "192.0.2.10")
	assert.Equal(t, "100.64.0.8", status.OverlayIP)
	assert.Equal(t, 9091, status.AgentPort)
}
