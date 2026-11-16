package agentproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/apiserver"
	"NYCU-SDC/caravanserai/internal/agentauth"
	"NYCU-SDC/caravanserai/internal/server/agentdialer"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type fakeProjects map[string]*v1.Project

func (f fakeProjects) GetProject(_ context.Context, name string) (*v1.Project, error) {
	if p, ok := f[name]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

type fakeNodes map[string]*v1.Node

func (f fakeNodes) GetNode(_ context.Context, name string) (*v1.Node, error) {
	if n, ok := f[name]; ok {
		return n, nil
	}
	return nil, store.ErrNotFound
}

// agentRoutes are what a fake Agent serves behind the real agent apiserver,
// so every request the proxy sends must carry a valid signature for node.
type agentRoutes func(mux *http.ServeMux)

func (f agentRoutes) RegisterRoutes(mux *http.ServeMux) { f(mux) }

type env struct {
	t       *testing.T
	signer  *agentauth.Signer
	server  *httptest.Server
	nodes   fakeNodes
	handler *Handler
	// lastAgentRequest is the last request a fake Agent route saw.
	lastAgentRequest chan *http.Request
}

func newSigner(t *testing.T) *agentauth.Signer {
	t.Helper()
	key, err := agentauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := agentauth.SignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, signer: newSigner(t), nodes: fakeNodes{}, lastAgentRequest: make(chan *http.Request, 16)}
	projects := fakeProjects{
		"web":     {ObjectMeta: v1.ObjectMeta{Name: "web"}, Status: v1.ProjectStatus{NodeRef: "node-a"}},
		"pending": {ObjectMeta: v1.ObjectMeta{Name: "pending"}},
	}
	dialer := agentdialer.New(agentdialer.Config{Nodes: e.nodes, Signer: e.signer})
	pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
	e.handler = NewHandler(zap.NewNop(), projects, dialer, e.signer, pw)
	e.handler.headerTimeout = 2 * time.Second
	mux := http.NewServeMux()
	e.handler.RegisterRoutes(mux, middleware.NewSet())
	e.server = httptest.NewServer(mux)
	t.Cleanup(e.server.Close)
	return e
}

// startAgent starts a fake Agent for node that checks signatures with key.
func (e *env) startAgent(node string, key *agentauth.Signer, routes agentRoutes) {
	e.t.Helper()
	keys := &agentauth.KeyHolder{}
	keys.Set(key.PublicKey())
	srv := apiserver.New(zap.NewNop(), agentauth.NewVerifier(node, keys))
	srv.Register(routes)
	agent := httptest.NewServer(srv.Handler())
	e.t.Cleanup(agent.Close)
	u, _ := url.Parse(agent.URL)
	host, port, _ := strings.Cut(u.Host, ":")
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	e.nodes[node] = &v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: node},
		Status:     v1.NodeStatus{Network: v1.NodeNetworkStatus{OverlayIP: host, AgentPort: p}},
	}
}

// logsAgent answers the logs route with "line from <node>" and records the
// request.
func (e *env) logsAgent(node string) agentRoutes {
	return func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/logs/{project}/{service}", func(w http.ResponseWriter, r *http.Request) {
			e.lastAgentRequest <- r
			_, _ = fmt.Fprintf(w, "line from %s\n", node)
		})
	}
}

func (e *env) get(path string, header http.Header) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.server.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestLogs_ReachesTheAssignedNodeWithASignedRequest(t *testing.T) {
	e := newEnv(t)
	e.startAgent("node-a", e.signer, e.logsAgent("node-a"))

	resp, body := e.get("/api/v1/projects/web/services/app/logs?follow=true&tail=5&node=&evil=1",
		http.Header{"Authorization": {"Bearer caller-token"}})
	if resp.StatusCode != http.StatusOK || body != "line from node-a\n" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	got := <-e.lastAgentRequest
	if got.URL.Path != "/api/v1/logs/web/app" {
		t.Errorf("agent path = %q", got.URL.Path)
	}
	if q := got.URL.Query(); q.Get("follow") != "true" || q.Get("tail") != "5" || q.Has("evil") || q.Has("node") {
		t.Errorf("agent query = %q, want only follow and tail", got.URL.RawQuery)
	}
	if a := got.Header.Get("Authorization"); a != "" {
		t.Errorf("the caller's token reached the agent: %q", a)
	}
}

func TestLogs_NodeOverride(t *testing.T) {
	e := newEnv(t)
	e.startAgent("node-a", e.signer, e.logsAgent("node-a"))
	e.startAgent("node-b", e.signer, e.logsAgent("node-b"))

	resp, body := e.get("/api/v1/projects/web/services/app/logs?node=node-b", nil)
	if resp.StatusCode != http.StatusOK || body != "line from node-b\n" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	// Also for a Project assigned nowhere.
	resp, body = e.get("/api/v1/projects/pending/services/app/logs?node=node-b", nil)
	if resp.StatusCode != http.StatusOK || body != "line from node-b\n" {
		t.Fatalf("pending project with ?node=: status %d body %q", resp.StatusCode, body)
	}
}

func TestLogs_Refusals(t *testing.T) {
	e := newEnv(t)
	e.startAgent("node-a", e.signer, e.logsAgent("node-a"))
	e.nodes["no-address"] = &v1.Node{ObjectMeta: v1.ObjectMeta{Name: "no-address"}}

	cases := []struct {
		name, path string
		status     int
		detail     string
	}{
		{"missing project", "/api/v1/projects/nope/services/app/logs", http.StatusNotFound, `project \"nope\"`},
		{"unassigned project", "/api/v1/projects/pending/services/app/logs", http.StatusConflict, "not assigned to a node"},
		{"missing node", "/api/v1/projects/web/services/app/logs?node=ghost", http.StatusNotFound, `node \"ghost\"`},
		{"node without address", "/api/v1/projects/web/services/app/logs?node=no-address", http.StatusConflict, "no Status.Network.OverlayIP"},
		{"invalid node name", "/api/v1/projects/web/services/app/logs?node=Not_A_Name", http.StatusBadRequest, ""},
		{"invalid port", "/api/v1/projects/web/services/app/forward/70000", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.get(tc.path, nil)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.detail) {
				t.Fatalf("status %d body %s; want %d containing %q", resp.StatusCode, body, tc.status, tc.detail)
			}
		})
	}
}

// An Agent that trusts another key refuses the server. The caller gets 502
// naming the signature, not the Agent's 401, which would read as its own
// token being refused.
func TestLogs_AgentRefusesTheSignature(t *testing.T) {
	e := newEnv(t)
	e.startAgent("node-a", newSigner(t), e.logsAgent("node-a"))

	resp, body := e.get("/api/v1/projects/web/services/app/logs", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "refused cara-server's signature") ||
		!strings.Contains(body, "signature does not verify") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestLogs_AgentThatNeverAnswersFailsInBoundedTime(t *testing.T) {
	e := newEnv(t)
	e.handler.headerTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	e.startAgent("node-a", e.signer, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/logs/{project}/{service}", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
	})

	start := time.Now()
	resp, body := e.get("/api/v1/projects/web/services/app/logs", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "no answer within 200ms") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s", took)
	}
}

// A followed log arrives line by line, not when the stream ends.
func TestLogs_StreamsAsTheAgentWrites(t *testing.T) {
	e := newEnv(t)
	e.handler.headerTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	e.startAgent("node-a", e.signer, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/logs/{project}/{service}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "first\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			_, _ = io.WriteString(w, "second\n")
		})
	})

	resp, err := http.Get(e.server.URL + "/api/v1/projects/web/services/app/logs?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewReader(resp.Body)
	first, err := lines.ReadString('\n')
	if err != nil || first != "first\n" {
		t.Fatalf("first line = %q, %v", first, err)
	}
	// Past the header timeout: a started stream is not cut.
	time.Sleep(400 * time.Millisecond)
	close(release)
	second, err := lines.ReadString('\n')
	if err != nil || second != "second\n" {
		t.Fatalf("second line = %q, %v", second, err)
	}
}

func TestForward_TunnelsAWebSocket(t *testing.T) {
	e := newEnv(t)
	upgrader := websocket.Upgrader{}
	e.startAgent("node-a", e.signer, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/forward/{project}/{service}/{port}", func(w http.ResponseWriter, r *http.Request) {
			e.lastAgentRequest <- r
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			for {
				kind, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				_ = c.WriteMessage(kind, append([]byte("echo:"), msg...))
			}
		})
	})

	wsURL := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/api/v1/projects/web/services/db/forward/5432"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer caller-token"}})
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial: %v %s", err, body)
	}
	defer func() { _ = conn.Close() }()

	got := <-e.lastAgentRequest
	if got.URL.Path != "/api/v1/forward/web/db/5432" || got.Header.Get("Authorization") != "" {
		t.Errorf("agent saw %s with Authorization %q", got.URL.Path, got.Header.Get("Authorization"))
	}

	// Past the signature's lifetime would take 30s; the point here is the
	// tunnel carries data both ways after the upgrade.
	for _, msg := range []string{"hello", "again"} {
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, reply, err := conn.ReadMessage()
		if err != nil || string(reply) != "echo:"+msg {
			t.Fatalf("reply = %q, %v", reply, err)
		}
	}
}

func TestSigningKey(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get("/api/v1/agent-signing-key", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got v1.AgentSigningKey
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	key, err := agentauth.DecodePublicKey(got.PublicKey)
	if err != nil || !key.Equal(e.signer.PublicKey()) {
		t.Fatalf("key = %v, %v", key, err)
	}
}
