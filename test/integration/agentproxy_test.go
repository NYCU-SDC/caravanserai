//go:build e2e

package integration

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent"
	agentapiserver "NYCU-SDC/caravanserai/internal/agent/apiserver"
	"NYCU-SDC/caravanserai/internal/agentauth"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/test/integration/testhelper"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type stubAgentRoutes func(*http.ServeMux)

func (f stubAgentRoutes) RegisterRoutes(mux *http.ServeMux) { f(mux) }

// startSignedAgent runs the real agent API for node, with stand-in logs and
// forward routes. Like cara-agent, it fetches the key it checks requests with
// from the server, using a Node token. It returns the agent's URL.
func startSignedAgent(t *testing.T, node string) string {
	t.Helper()
	client := agent.NewClient(zap.NewNop(), suite.serverURL, node)
	client.SetToken(issueToken(t, v1.APITokenKindNode, node))
	key, err := client.FetchSigningKey(context.Background())
	require.NoError(t, err, "fetch the signing key with a Node token")
	keys := &agentauth.KeyHolder{}
	keys.Set(key)

	upgrader := websocket.Upgrader{}
	srv := agentapiserver.New(zap.NewNop(), agentauth.NewVerifier(node, keys))
	srv.Register(stubAgentRoutes(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/logs/{project}/{service}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "%s/%s on %s tail=%s\n", r.PathValue("project"), r.PathValue("service"), node, r.URL.Query().Get("tail"))
		})
		mux.HandleFunc("GET /api/v1/forward/{project}/{service}/{port}", func(w http.ResponseWriter, r *http.Request) {
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(websocket.BinaryMessage, []byte(node+":"+r.PathValue("port")+":"+string(msg)))
		})
	}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// pointNodeAt registers node with agentURL as its agent address.
func pointNodeAt(t *testing.T, node, agentURL string) {
	t.Helper()
	drainBody(doRequest(t, http.MethodDelete, "/api/v1/nodes/"+node, nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes", mustMarshal(t, v1.Node{ObjectMeta: v1.ObjectMeta{Name: node}}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	host, port := hostPort(t, agentURL)
	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+node+"/heartbeat", mustMarshal(t, map[string]any{
		"state":   string(v1.NodeStateReady),
		"network": map[string]any{"overlayIP": host, "agentPort": port},
	}))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)
	t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/nodes/"+node, nil)) })
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer drainBody(resp)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// TestAgentProxy covers CARA-139 end to end against the real server: logs and
// port-forward reach the agent only through cara-server, which signs for the
// Node it dials, and the agent takes nothing else.
func TestAgentProxy(t *testing.T) {
	const (
		nodeName = "proxy-node-a"
		project  = "proxy-project"
	)
	agentURL := startSignedAgent(t, nodeName)
	pointNodeAt(t, nodeName, agentURL)

	drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+project+"?force=true", nil))
	resp := doRequest(t, http.MethodPost, "/api/v1/projects", mustMarshal(t, v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: project},
		Spec:       v1.ProjectSpec{Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}},
	}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/projects/"+project+"?force=true", nil)) })
	require.NoError(t, adapter.NewProjectStoreAdapter(suite.store).SetProjectScheduled(context.Background(),
		project, testhelper.UID(t, suite.store, project), nodeName))

	t.Run("logs through the server", func(t *testing.T) {
		resp := doRequest(t, http.MethodGet, "/api/v1/projects/"+project+"/services/web/logs?tail=3", nil)
		body := readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Equal(t, project+"/web on "+nodeName+" tail=3\n", body)
	})

	t.Run("port-forward through the server", func(t *testing.T) {
		wsURL := "ws" + strings.TrimPrefix(suite.serverURL, "http") + "/api/v1/projects/" + project + "/services/web/forward/8080"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + testAdminToken}})
		if resp != nil && resp.Body != nil {
			defer drainBody(resp)
		}
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("ping")))
		_, reply, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, nodeName+":8080:ping", string(reply))
	})

	t.Run("probe uses the signed path", func(t *testing.T) {
		resp := doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/probe", nil)
		body := readAll(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, `"ok":true`)
	})

	t.Run("the agent refuses a request that did not come through the server", func(t *testing.T) {
		for _, path := range []string{"/api/v1/logs/" + project + "/web", "/api/v1/forward/" + project + "/web/8080", agentapiserver.ProbePath} {
			resp, err := http.Get(agentURL + path)
			require.NoError(t, err)
			body := readAll(t, resp)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s: %s", path, body)
		}
		resp, err := http.Get(agentURL + "/healthz")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		drainBody(resp)
	})

	t.Run("a Node token cannot use the proxy", func(t *testing.T) {
		bearer := "Bearer " + issueToken(t, v1.APITokenKindNode, nodeName)
		resp := requestAs(t, bearer, http.MethodGet, "/api/v1/projects/"+project+"/services/web/logs", nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		drainBody(resp)
	})
}

// TestAgentProxy_SignatureForAnotherNode: when a Node's address leads to
// another Node's agent, that agent refuses the request, because the server
// signed it for the Node it meant to reach.
func TestAgentProxy_SignatureForAnotherNode(t *testing.T) {
	agentOfB := startSignedAgent(t, "proxy-node-b")
	pointNodeAt(t, "proxy-node-c", agentOfB)

	resp := doRequest(t, http.MethodGet, "/api/v1/projects/anything/services/web/logs?node=proxy-node-c", nil)
	body := readAll(t, resp)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	assert.Contains(t, body, "another node")
}

// TestAgentSigningKeyIsKept: the server reads the key it stored instead of
// making a new one, so a restarted or new leader signs with the key agents
// already hold.
func TestAgentSigningKeyIsKept(t *testing.T) {
	raw, err := suite.store.AgentSigningKey(context.Background(), func() ([]byte, error) {
		t.Fatal("a key exists; none should be generated")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, ed25519.PrivateKey(raw).Public().(ed25519.PublicKey).Equal(suite.signer.PublicKey()))
}
