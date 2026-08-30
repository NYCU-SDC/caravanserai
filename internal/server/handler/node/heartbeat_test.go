package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// hbNodeStore is a minimal NodeStore for heartbeat tests: it returns a fixed
// node from GetNode and records the status passed to UpdateNodeStatus.
type hbNodeStore struct {
	node        *v1.Node
	updated     bool
	lastStatus  v1.NodeStatus
	updateErr   error
	getNotFound bool
}

func (s *hbNodeStore) GetNode(_ context.Context, name string) (*v1.Node, error) {
	if s.getNotFound {
		return nil, store.ErrNotFound
	}
	return s.node, nil
}
func (s *hbNodeStore) UpdateNodeStatus(_ context.Context, _ string, status v1.NodeStatus) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = true
	s.lastStatus = status
	return nil
}
func (s *hbNodeStore) CreateNode(context.Context, *v1.Node) error     { return nil }
func (s *hbNodeStore) UpdateNode(context.Context, *v1.Node) error     { return nil }
func (s *hbNodeStore) UpdateNodeSpec(context.Context, *v1.Node) error { return nil }
func (s *hbNodeStore) ListNodes(context.Context) ([]*v1.Node, error)  { return nil, nil }
func (s *hbNodeStore) DeleteNode(context.Context, string) error       { return nil }

// fakeKeyValidator is a configurable PreAuthKeyValidator.
type fakeKeyValidator struct {
	mapping     *store.PreAuthKey
	getErr      error // e.g. store.ErrNotFound
	markCalls   int
	markKeyHash string
	markIP      string
	markErr     error
}

func (f *fakeKeyValidator) GetPreAuthKeyByHash(_ context.Context, _ string) (*store.PreAuthKey, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.mapping, nil
}
func (f *fakeKeyValidator) MarkPreAuthKeyUsed(_ context.Context, keyHash, usedByIP string, _ time.Time) error {
	f.markCalls++
	f.markKeyHash = keyHash
	f.markIP = usedByIP
	return f.markErr
}

func newHeartbeatServer(t *testing.T, ns store.NodeStore, keys PreAuthKeyValidator) *httptest.Server {
	t.Helper()
	pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
	h := NewHandler(zap.NewNop(), ns, nil, keys, nil, pw)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, middleware.NewSet())
	return httptest.NewServer(mux)
}

func postHeartbeat(t *testing.T, url, node, keyRef, overlayIP string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"keyRef":  keyRef,
		"network": v1.NodeNetworkStatus{OverlayIP: overlayIP},
	})
	resp, err := http.Post(url+"/api/v1/nodes/"+node+"/heartbeat", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	return resp
}

func existingNode(name string) *v1.Node {
	return &v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Status:     v1.NodeStatus{State: v1.NodeStateReady},
	}
}

func TestHeartbeat_KeyRef_ActiveSameNode_MarksUsed(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{
		KeyHash: "hash-a", CaraNodeName: "agent-a", State: store.PreAuthKeyStateActive,
	}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "hash-a", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated, "status should be updated on a valid heartbeat")
	assert.Equal(t, 1, keys.markCalls)
	assert.Equal(t, "hash-a", keys.markKeyHash)
	assert.Equal(t, "100.64.0.5", keys.markIP)
}

func TestHeartbeat_KeyRef_WrongNode_Rejected(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-b")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{
		KeyHash: "hash-a", CaraNodeName: "agent-a", State: store.PreAuthKeyStateActive,
	}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	// A key issued for agent-a is used to claim agent-b.
	resp := postHeartbeat(t, srv.URL, "agent-b", "hash-a", "100.64.0.9")
	defer resp.Body.Close()

	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.False(t, ns.updated, "a wrong-node heartbeat must not mutate the node")
	assert.Equal(t, 0, keys.markCalls, "must not consume a key it does not own")
}

func TestHeartbeat_KeyRef_Expired_Rejected(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{
		KeyHash: "hash-a", CaraNodeName: "agent-a", State: store.PreAuthKeyStateActive,
		Expiration: time.Now().Add(-1 * time.Hour),
	}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "hash-a", "100.64.0.5")
	defer resp.Body.Close()

	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.False(t, ns.updated)
	assert.Equal(t, 0, keys.markCalls)
}

func TestHeartbeat_KeyRef_Unknown_Allowed(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{getErr: store.ErrNotFound}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	// Key created out of band (not via caractl): allowed, but not bound.
	resp := postHeartbeat(t, srv.URL, "agent-a", "unknown-hash", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated, "an unknown key still lets the heartbeat through")
	assert.Equal(t, 0, keys.markCalls)
}

func TestHeartbeat_KeyRef_AlreadyUsedSameNode_Idempotent(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{
		KeyHash: "hash-a", CaraNodeName: "agent-a", State: store.PreAuthKeyStateUsed,
	}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "hash-a", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated)
	assert.Equal(t, 0, keys.markCalls, "an already-used key is not marked again")
}

func TestHeartbeat_KeyRef_UsedAndExpiredSameNode_Allowed(t *testing.T) {
	// A key that was already consumed proves the node joined legitimately.
	// Expiry only gates the initial join, so once the key is used its TTL
	// elapsing must not start failing the node's heartbeats.
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{
		KeyHash: "hash-a", CaraNodeName: "agent-a", State: store.PreAuthKeyStateUsed,
		Expiration: time.Now().Add(-1 * time.Hour),
	}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "hash-a", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated, "a joined node's heartbeats must not fail after its key expires")
	assert.Equal(t, 0, keys.markCalls, "an already-used key is not marked again")
}

func TestHeartbeat_NoKeyRef_SkipsValidation(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	keys := &fakeKeyValidator{mapping: &store.PreAuthKey{CaraNodeName: "other"}}
	srv := newHeartbeatServer(t, ns, keys)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated)
	assert.Equal(t, 0, keys.markCalls, "no keyRef means the validator is never consulted for marking")
}

func TestHeartbeat_NilValidator_Works(t *testing.T) {
	ns := &hbNodeStore{node: existingNode("agent-a")}
	srv := newHeartbeatServer(t, ns, nil)
	defer srv.Close()

	resp := postHeartbeat(t, srv.URL, "agent-a", "hash-a", "100.64.0.5")
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.True(t, ns.updated)
}
