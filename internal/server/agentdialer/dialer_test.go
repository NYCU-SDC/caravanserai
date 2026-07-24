package agentdialer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"
)

// fakeNodeLookup is a table-driven NodeAddressLookup used by all tests below.
// Two nodes can be pre-populated so a test can verify that a mutation between
// two Client() calls is observed on the second call (AC: "overlay address
// changed between calls").
type fakeNodeLookup struct {
	byName map[string]*v1.Node
	err    error
}

func (f *fakeNodeLookup) GetNode(_ context.Context, name string) (*v1.Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	n, ok := f.byName[name]
	if !ok {
		return nil, fmt.Errorf("stub: %w", store.ErrNotFound)
	}
	return n, nil
}

func node(name, ip string, port int) *v1.Node {
	n := &v1.Node{ObjectMeta: v1.ObjectMeta{Name: name}}
	n.Status.Network.OverlayIP = ip
	n.Status.Network.AgentPort = port
	return n
}

func TestDialer_Client_OverlayAddressPresent(t *testing.T) {
	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{
		"pve1": node("pve1", "100.64.0.5", 9090),
	}}
	d := New(Config{Nodes: lookup})

	client, base, err := d.Client(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil *http.Client")
	}
	if want := "http://100.64.0.5:9090"; base != want {
		t.Errorf("base URL = %q, want %q", base, want)
	}
	if client.Timeout != DefaultTimeout {
		t.Errorf("client.Timeout = %v, want %v", client.Timeout, DefaultTimeout)
	}
}

func TestDialer_Client_DefaultsAgentPortWhenZero(t *testing.T) {
	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{
		"pve1": node("pve1", "100.64.0.5", 0),
	}}
	d := New(Config{Nodes: lookup})

	_, base, err := d.Client(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fmt.Sprintf("http://100.64.0.5:%d", DefaultAgentPort)
	if base != want {
		t.Errorf("base URL = %q, want %q", base, want)
	}
}

func TestDialer_Client_OverlayAddressMissing(t *testing.T) {
	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{
		"pve1": node("pve1", "", 9090),
	}}
	d := New(Config{Nodes: lookup})

	_, _, err := d.Client(context.Background(), "pve1")
	if err == nil {
		t.Fatal("expected error when Status.Network.OverlayIP is empty")
	}
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Errorf("error = %v, want wrapping ErrNodeUnreachable", err)
	}
}

func TestDialer_Client_NodeNotFound(t *testing.T) {
	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{}}
	d := New(Config{Nodes: lookup})

	_, _, err := d.Client(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for missing node")
	}
	if !errors.Is(err, ErrNodeLookup) {
		t.Errorf("error = %v, want wrapping ErrNodeLookup", err)
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("error = %v, want wrapping store.ErrNotFound", err)
	}
}

func TestDialer_Client_LookupError(t *testing.T) {
	sentinel := errors.New("db exploded")
	lookup := &fakeNodeLookup{err: sentinel}
	d := New(Config{Nodes: lookup})

	_, _, err := d.Client(context.Background(), "pve1")
	if !errors.Is(err, ErrNodeLookup) || !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want wrapping both ErrNodeLookup and the underlying error", err)
	}
}

func TestDialer_Client_ReflectsAddressChangeBetweenCalls(t *testing.T) {
	// AC: overlay address changed between calls — no caching allowed.
	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{
		"pve1": node("pve1", "100.64.0.5", 9090),
	}}
	d := New(Config{Nodes: lookup})

	_, base1, err := d.Client(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if base1 != "http://100.64.0.5:9090" {
		t.Fatalf("first call base URL = %q", base1)
	}

	// Simulate an agent re-joining the overlay with a new IP.
	lookup.byName["pve1"] = node("pve1", "100.64.0.9", 9090)

	_, base2, err := d.Client(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if base2 != "http://100.64.0.9:9090" {
		t.Errorf("second call base URL = %q; dialer must not cache addresses", base2)
	}
}

func TestDialer_Client_UsesInjectedTransport(t *testing.T) {
	// Verify that the returned http.Client dials through the injected
	// Transport. We hijack requests to a fake in-process handler regardless
	// of the URL host and assert the response body flows back.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("stub-agent"))
	}))
	defer stub.Close()

	// A transport that ignores the incoming Request.URL host and rewrites
	// every request to the stub server. Simulates what a tsnet-backed
	// RoundTripper would do for the overlay host name.
	rewriting := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = stub.Listener.Addr().String()
		clone.Host = clone.URL.Host
		return http.DefaultTransport.RoundTrip(clone)
	})

	lookup := &fakeNodeLookup{byName: map[string]*v1.Node{
		"pve1": node("pve1", "100.64.0.5", 9090),
	}}
	d := New(Config{Nodes: lookup, Transport: rewriting})

	client, base, err := d.Client(context.Background(), "pve1")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}

	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
