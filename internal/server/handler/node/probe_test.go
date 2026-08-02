package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/agentdialer"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// fakeDialer returns whatever the test provides. It implements
// agentdialer.Dialer without going near a real HTTP transport, so probe
// tests can drive the handler in-process.
type fakeDialer struct {
	client  *http.Client
	baseURL string
	err     error
}

func (f *fakeDialer) Client(_ context.Context, _ string) (*http.Client, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.client, f.baseURL, nil
}

// fakeNodeStore satisfies store.NodeStore but only implements the methods
// the probe handler indirectly needs (none — the Dialer is faked).
type fakeNodeStore struct{}

func (fakeNodeStore) CreateNode(context.Context, *v1.Node) error     { return nil }
func (fakeNodeStore) UpdateNode(context.Context, *v1.Node) error     { return nil }
func (fakeNodeStore) UpdateNodeSpec(context.Context, *v1.Node) error { return nil }
func (fakeNodeStore) ListNodes(context.Context) ([]*v1.Node, error)  { return nil, nil }
func (fakeNodeStore) GetNode(context.Context, string) (*v1.Node, error) {
	return nil, store.ErrNotFound
}
func (fakeNodeStore) DeleteNode(context.Context, string) error { return nil }
func (fakeNodeStore) UpdateNodeStatus(context.Context, string, v1.NodeStatus) error {
	return nil
}

type fakeProjectLister struct{}

func (fakeProjectLister) ListProjectsByNodeRef(context.Context, string, []v1.ProjectPhase) ([]*v1.Project, error) {
	return nil, nil
}

func newTestHandler(t *testing.T, dialer agentdialer.Dialer) http.Handler {
	t.Helper()
	pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
	h := NewHandler(zap.NewNop(), fakeNodeStore{}, fakeProjectLister{}, dialer, pw)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, middleware.NewSet())
	return mux
}

func TestProbe_HappyPath(t *testing.T) {
	// Fake agent returning 200 on /healthz.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("agent got path %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer agent.Close()

	dialer := &fakeDialer{
		client:  agent.Client(),
		baseURL: agent.URL,
	}
	srv := httptest.NewServer(newTestHandler(t, dialer))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/pve1/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("probe status = %d, body = %s", resp.StatusCode, body)
	}

	var got probeResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("OK = false; want true (result=%+v)", got)
	}
	if got.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", got.StatusCode)
	}
	if got.Address != agent.URL {
		t.Errorf("Address = %q, want %q", got.Address, agent.URL)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
}

func TestProbe_AgentReturnsNon2xx(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer agent.Close()

	dialer := &fakeDialer{client: agent.Client(), baseURL: agent.URL}
	srv := httptest.NewServer(newTestHandler(t, dialer))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/pve1/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("outer status = %d, want 200 (probe reports agent status in body)", resp.StatusCode)
	}
	var got probeResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.OK {
		t.Errorf("OK = true; want false when agent returns 500")
	}
	if got.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", got.StatusCode)
	}
	if !strings.Contains(got.Error, "500") {
		t.Errorf("Error = %q, want it to mention status 500", got.Error)
	}
}

func TestProbe_NodeUnreachable(t *testing.T) {
	dialer := &fakeDialer{err: fmt.Errorf("%w: no address", agentdialer.ErrNodeUnreachable)}
	srv := httptest.NewServer(newTestHandler(t, dialer))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/pve1/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409 for unreachable node; body=%s", resp.StatusCode, body)
	}
	var got probeResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.OK {
		t.Error("OK = true; want false")
	}
	if got.Error == "" {
		t.Error("Error is empty; want a message explaining the missing address")
	}
}

func TestProbe_NodeNotFound(t *testing.T) {
	dialer := &fakeDialer{err: fmt.Errorf("%w: %w", agentdialer.ErrNodeLookup, store.ErrNotFound)}
	srv := httptest.NewServer(newTestHandler(t, dialer))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/missing/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404; body=%s", resp.StatusCode, body)
	}
}

func TestProbe_TransportError(t *testing.T) {
	// Point the base URL at a definitely-closed port so Do() fails fast.
	dialer := &fakeDialer{
		client:  &http.Client{},
		baseURL: "http://127.0.0.1:1", // reserved, expected to refuse
	}
	srv := httptest.NewServer(newTestHandler(t, dialer))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/pve1/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("outer status = %d, want 200", resp.StatusCode)
	}
	var got probeResult
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.OK {
		t.Error("OK = true; want false when transport fails")
	}
	if got.Error == "" {
		t.Error("Error empty; want dial error message")
	}
}

func TestProbe_NoDialer(t *testing.T) {
	// A handler constructed without a dialer should return 500 rather than
	// panic — safety net for tests that omit the dependency.
	pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
	h := NewHandler(zap.NewNop(), fakeNodeStore{}, fakeProjectLister{}, nil, pw)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, middleware.NewSet())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/nodes/pve1/probe", "application/json", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 500 {
		t.Errorf("status = %d, want 5xx when dialer missing", resp.StatusCode)
	}
}

var _ store.NodeStore = fakeNodeStore{}
var _ = errors.Is // silence linter until helper needed
