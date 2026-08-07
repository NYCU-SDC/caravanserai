//go:build e2e

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeAgent stands in for cara-agent's HTTP server. It serves /healthz with
// the configured response and lets the test control the response so we can
// exercise both the happy path and error propagation.
type probeAgent struct {
	server     *httptest.Server
	respStatus int
}

func startProbeAgent(t *testing.T, status int) *probeAgent {
	t.Helper()
	p := &probeAgent{respStatus: status}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probeAgent got unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(p.respStatus)
		_, _ = w.Write([]byte("stub-agent"))
	}))
	return p
}

// hostPort splits an httptest URL like http://127.0.0.1:53812 into
// ("127.0.0.1", 53812).
func hostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(url, "http://")
	host, port, err := net.SplitHostPort(trimmed)
	require.NoError(t, err, "split %q", trimmed)
	p, err := strconv.Atoi(port)
	require.NoError(t, err, "parse port %q", port)
	return host, p
}

// TestProbeNode_HappyPath exercises the full server→agent probe cycle:
//
//  1. Create a Node.
//  2. Heartbeat it with a stored agent address that points at our mock agent.
//  3. POST /api/v1/nodes/{name}/probe.
//  4. Assert the response reports OK=true with the correct address.
//
// This validates that:
//   - the agent dialer resolves the Node's Status.Network address from the
//     store on every call,
//   - the probe handler dials that address via the injected transport, and
//   - a healthy /healthz response is reported as OK to the caller.
//
// Note: this test uses the default passthrough transport (net/http). It proves
// the server-side dialer path end to end, but not a real Headscale data path.
// Once overlay support lands (CARA-48 + CARA-55), a second variant can run
// with tsnet.Server-backed transport against a Headscale test container; the
// server-side probe logic is identical either way.
func TestProbeNode_HappyPath(t *testing.T) {
	const nodeName = "e2e-probe-happy"

	agent := startProbeAgent(t, http.StatusOK)
	defer agent.server.Close()
	agentHost, agentPort := hostPort(t, agent.server.URL)

	// 1. Create the Node.
	createBody := mustMarshal(t, v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: nodeName},
		Spec:       v1.NodeSpec{Hostname: "probe-host"},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create node")
	drainBody(resp)

	// 2. Heartbeat with the mock agent's address so the dialer has something
	//    to resolve to.
	hbBody := mustMarshal(t, map[string]any{
		"state": string(v1.NodeStateReady),
		"network": map[string]any{
			"overlayIP": agentHost,
			"agentPort": agentPort,
		},
	})
	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/heartbeat", hbBody)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "heartbeat")
	drainBody(resp)

	// 3. Probe.
	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/probe", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "probe: expected 200")

	var got struct {
		OK         bool   `json:"ok"`
		StatusCode int    `json:"statusCode"`
		LatencyMs  int64  `json:"latencyMs"`
		Address    string `json:"address"`
		Error      string `json:"error,omitempty"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))

	assert.True(t, got.OK, "probe should report OK; got %+v", got)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Equal(t, fmt.Sprintf("http://%s:%d", agentHost, agentPort), got.Address)
	assert.Empty(t, got.Error)
}

// TestProbeNode_Unreachable exercises the case where the Node record does not
// yet carry a Status.Network.IP (i.e. the agent has never heartbeated with an
// address). The server should refuse the probe with 409 Conflict.
func TestProbeNode_Unreachable(t *testing.T) {
	const nodeName = "e2e-probe-unreachable"

	// Create the Node but do NOT send a heartbeat with a network address.
	createBody := mustMarshal(t, v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: nodeName},
		Spec:       v1.NodeSpec{Hostname: "probe-host"},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create node")
	drainBody(resp)

	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/probe", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode,
		"probe on unreachable node: expected 409")
	drainBody(resp)
}

// TestProbeNode_NodeNotFound: probing a node that was never registered must
// return 404.
func TestProbeNode_NodeNotFound(t *testing.T) {
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes/e2e-probe-does-not-exist/probe", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "probe on missing node: expected 404")
	drainBody(resp)
}

// TestProbeNode_AgentReturnsError: agent up but /healthz returns non-2xx —
// server should report OK=false with the observed status code.
func TestProbeNode_AgentReturnsError(t *testing.T) {
	const nodeName = "e2e-probe-agent-error"

	agent := startProbeAgent(t, http.StatusInternalServerError)
	defer agent.server.Close()
	agentHost, agentPort := hostPort(t, agent.server.URL)

	createBody := mustMarshal(t, v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: nodeName},
		Spec:       v1.NodeSpec{Hostname: "probe-host"},
	})
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create node")
	drainBody(resp)

	hbBody := mustMarshal(t, map[string]any{
		"state": string(v1.NodeStateReady),
		"network": map[string]any{
			"overlayIP": agentHost,
			"agentPort": agentPort,
		},
	})
	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/heartbeat", hbBody)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)

	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/probe", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		OK         bool   `json:"ok"`
		StatusCode int    `json:"statusCode"`
		Error      string `json:"error,omitempty"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.False(t, got.OK, "OK should be false when agent returns 5xx")
	assert.Equal(t, http.StatusInternalServerError, got.StatusCode)
	assert.NotEmpty(t, got.Error)
}
