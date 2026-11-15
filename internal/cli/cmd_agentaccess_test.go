package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

// fakeServer stands in for cara-server's proxy routes and records what
// caractl asked for.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

func (f *fakeServer) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the server")
	}
	return f.requests[len(f.requests)-1]
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "web":
			_ = json.NewEncoder(w).Encode(v1.Project{Status: v1.ProjectStatus{NodeRef: "node-a"}})
		case "pending":
			_ = json.NewEncoder(w).Encode(v1.Project{})
		default:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"title":"Not Found","status":404,"detail":"project not found"}`)
		}
	})
	mux.HandleFunc("GET /api/v1/projects/{name}/services/{service}/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "web" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"title":"Bad Gateway","status":502,"detail":"cannot reach the agent on node \"node-a\""}`)
			return
		}
		_, _ = fmt.Fprintf(w, "logs of %s\n", r.PathValue("service"))
	})
	mux.HandleFunc("GET /api/v1/projects/{name}/services/{service}/forward/{port}", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(websocket.BinaryMessage, append([]byte("echo:"), msg...))
		}
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func TestStreamLogs_GoesThroughTheServer(t *testing.T) {
	srv := newFakeServer(t)
	var out bytes.Buffer
	err := streamLogs(context.Background(), NewClientWithToken(srv.URL, "tok"), "web", "app", "node-b", true, "10", true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "logs of app\n" {
		t.Errorf("output = %q", out.String())
	}
	r := srv.last(t)
	if r.URL.Path != "/api/v1/projects/web/services/app/logs" {
		t.Errorf("path = %q", r.URL.Path)
	}
	q := r.URL.Query()
	if q.Get("follow") != "true" || q.Get("tail") != "10" || q.Get("timestamps") != "true" || q.Get("node") != "node-b" {
		t.Errorf("query = %q", r.URL.RawQuery)
	}
	if r.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
}

func TestStreamLogs_ReportsTheServersError(t *testing.T) {
	srv := newFakeServer(t)
	err := streamLogs(context.Background(), NewClient(srv.URL), "down", "app", "", false, "all", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `cannot reach the agent on node "node-a"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestNodeFlagRefusesAnAgentAddress(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "web/app", "--node", "10.0.0.5:9090"},
		{"port-forward", "web/app", "8080", "--node", "10.0.0.5:9090"},
	} {
		root := newTestRoot()
		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "agents accept no one else") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestParseServiceTarget(t *testing.T) {
	for _, bad := range []string{"web", "/app", "web/", "web/app/x"} {
		if _, _, err := parseServiceTarget(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if p, s, err := parseServiceTarget("web/app"); err != nil || p != "web" || s != "app" {
		t.Errorf("web/app = %q %q %v", p, s, err)
	}
}

func TestForwardURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://cara:8080":     "ws://cara:8080/api/v1/projects/web/services/db/forward/5432?node=node-b",
		"https://cara.example": "wss://cara.example/api/v1/projects/web/services/db/forward/5432?node=node-b",
	} {
		got, err := forwardURL(NewClient(base), "web", "db", "node-b", 5432)
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", base, got, err, want)
		}
	}
}

func TestPortForward_FailsBeforeListeningWhenTheProjectCannotBeReached(t *testing.T) {
	srv := newFakeServer(t)
	for project, want := range map[string]string{
		"missing": "project not found",
		"pending": "not assigned to a node",
	} {
		err := runPortForward(context.Background(), NewClient(srv.URL), "", project, "db", "", freePort(t), 5432, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", project, err, want)
		}
	}
}

func TestPortForward_TunnelsThroughTheServer(t *testing.T) {
	srv := newFakeServer(t)
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runPortForward(ctx, NewClient(srv.URL), "tok", "web", "db", "", port, 5432, io.Discard)
	}()

	var conn net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if conn, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len("echo:ping"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "echo:ping" {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	_ = conn.Close()

	r := srv.last(t)
	if r.URL.Path != "/api/v1/projects/web/services/db/forward/5432" || r.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("server saw %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "caractl", SilenceErrors: true}
	root.PersistentFlags().String("server", "http://127.0.0.1:1", "")
	root.AddCommand(NewLogsCmd(), NewPortForwardCmd())
	return root
}
