package overlay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/headscale"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeHeadscale is a configurable headscale.Client.
type fakeHeadscale struct {
	nodes       []headscale.Node
	listErr     error
	deleteErr   error
	deleteCalls int
	createKey   headscale.PreAuthKey
	createErr   error
	createCalls int
}

func (f *fakeHeadscale) CreatePreAuthKey(_ context.Context, _ headscale.CreatePreAuthKeyRequest) (headscale.PreAuthKey, error) {
	f.createCalls++
	if f.createErr != nil {
		return headscale.PreAuthKey{}, f.createErr
	}
	return f.createKey, nil
}

func (f *fakeHeadscale) ListNodes(_ context.Context) ([]headscale.Node, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.nodes, nil
}

func (f *fakeHeadscale) DeleteNode(_ context.Context, _ string) error {
	f.deleteCalls++
	return f.deleteErr
}

// fakeStore is a configurable NodeDeleter.
type fakeStore struct {
	calls         int
	failUntil     int  // return a transient error while calls <= failUntil
	persistentErr bool // always fail
	notFound      bool // return store.ErrNotFound
}

func (f *fakeStore) DeleteNode(_ context.Context, _ string) error {
	f.calls++
	if f.notFound {
		return store.ErrNotFound
	}
	if f.persistentErr {
		return errors.New("db is down")
	}
	if f.calls <= f.failUntil {
		return errors.New("transient db error")
	}
	return nil
}

func newTestServer(t *testing.T, hs headscale.Client, s NodeDeleter) *httptest.Server {
	t.Helper()
	pw := problem.NewWithMapping(serverhandler.NewProblemMapping())
	h := NewHandler(zap.NewNop(), hs, s, "cara-node", pw)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, middleware.NewSet())
	return httptest.NewServer(mux)
}

func TestCreatePreAuthKey_HappyPath(t *testing.T) {
	hs := &fakeHeadscale{createKey: headscale.PreAuthKey{Key: "secret-123"}}
	srv := newTestServer(t, hs, &fakeStore{})
	defer srv.Close()

	body, _ := json.Marshal(v1.CreatePreAuthKeyRequest{Node: "agent-a", TTL: "1h"})
	resp, err := http.Post(srv.URL+"/api/v1/overlay/preauth-keys", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var out v1.PreAuthKeyResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, "secret-123", out.Key)
	assert.Equal(t, 1, hs.createCalls)
}

func TestCreatePreAuthKey_InvalidTTL(t *testing.T) {
	hs := &fakeHeadscale{}
	srv := newTestServer(t, hs, &fakeStore{})
	defer srv.Close()

	body, _ := json.Marshal(v1.CreatePreAuthKeyRequest{Node: "agent-a", TTL: "nonsense"})
	resp, err := http.Post(srv.URL+"/api/v1/overlay/preauth-keys", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.Equal(t, 0, hs.createCalls, "must not call Headscale on a bad request")
}

func TestListNodes_HappyPath(t *testing.T) {
	hs := &fakeHeadscale{nodes: []headscale.Node{
		{ID: "1", Name: "agent-a", IPAddresses: []string{"100.64.0.1"}, Online: true},
	}}
	srv := newTestServer(t, hs, &fakeStore{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/overlay/nodes")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out v1.OverlayNodeList
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Nodes, 1)
	assert.Equal(t, "agent-a", out.Nodes[0].Name)
	assert.True(t, out.Nodes[0].Online)
}

func TestRevoke_HappyPath(t *testing.T) {
	hs := &fakeHeadscale{nodes: []headscale.Node{{ID: "5", Name: "agent-a"}}}
	st := &fakeStore{}
	res := doRevoke(t, hs, st, "agent-a")

	assert.True(t, res.HeadscaleRemoved)
	assert.True(t, res.StoreRemoved)
	assert.False(t, res.Drift)
	assert.Equal(t, 1, hs.deleteCalls)
	assert.Equal(t, 1, st.calls)
}

func TestRevoke_Idempotent_HeadscaleAbsent(t *testing.T) {
	// Node is not on Headscale (already gone); revoke still cleans the store.
	hs := &fakeHeadscale{nodes: nil}
	st := &fakeStore{}
	res := doRevoke(t, hs, st, "agent-a")

	assert.True(t, res.HeadscaleRemoved)
	assert.True(t, res.StoreRemoved)
	assert.False(t, res.Drift)
	assert.Equal(t, 0, hs.deleteCalls, "no Headscale delete when the node is already absent")
	assert.Equal(t, 1, st.calls)
}

func TestRevoke_StoreAlreadyGone(t *testing.T) {
	hs := &fakeHeadscale{nodes: []headscale.Node{{ID: "5", Name: "agent-a"}}}
	st := &fakeStore{notFound: true}
	res := doRevoke(t, hs, st, "agent-a")

	// ErrNotFound on the store side counts as removed (idempotent).
	assert.True(t, res.StoreRemoved)
	assert.False(t, res.Drift)
}

func TestRevoke_HeadscaleDeleteFails_StoreUntouched(t *testing.T) {
	// Drift direction one: Headscale delete fails, so the store must be left
	// untouched and the request must fail.
	hs := &fakeHeadscale{
		nodes:     []headscale.Node{{ID: "5", Name: "agent-a"}},
		deleteErr: errors.New("headscale 500"),
	}
	st := &fakeStore{}
	srv := newTestServer(t, hs, st)
	defer srv.Close()

	resp := deleteReq(t, srv.URL+"/api/v1/overlay/nodes/agent-a")
	defer resp.Body.Close()

	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.Equal(t, 0, st.calls, "store must not be touched when Headscale delete fails")
}

func TestRevoke_StorePersistentFailure_ReportsDrift(t *testing.T) {
	// Drift direction two: Headscale removed, but the store delete keeps
	// failing. The response reports drift loudly rather than hiding it.
	hs := &fakeHeadscale{nodes: []headscale.Node{{ID: "5", Name: "agent-a"}}}
	st := &fakeStore{persistentErr: true}
	res := doRevoke(t, hs, st, "agent-a")

	assert.True(t, res.HeadscaleRemoved)
	assert.False(t, res.StoreRemoved)
	assert.True(t, res.Drift)
	assert.NotEmpty(t, res.Message)
	assert.Equal(t, storeDeleteAttempts, st.calls, "store delete should be retried")
}

func TestRevoke_StoreTransientThenSuccess(t *testing.T) {
	// A transient store error clears on retry — no drift.
	hs := &fakeHeadscale{nodes: []headscale.Node{{ID: "5", Name: "agent-a"}}}
	st := &fakeStore{failUntil: storeDeleteAttempts - 1} // fail all but the last attempt
	res := doRevoke(t, hs, st, "agent-a")

	assert.True(t, res.StoreRemoved)
	assert.False(t, res.Drift)
	assert.Equal(t, storeDeleteAttempts, st.calls)
}

func TestNotConfigured(t *testing.T) {
	// A nil Headscale client means overlay admin is not configured; every
	// endpoint must report that rather than panic.
	srv := newTestServer(t, nil, &fakeStore{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/overlay/nodes")
	require.NoError(t, err)
	defer resp.Body.Close()
	// "Not configured" is a deployment gap, not a server fault, so it maps to
	// 503 Service Unavailable rather than a generic 500.
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// --- helpers ---------------------------------------------------------------

func doRevoke(t *testing.T, hs headscale.Client, s NodeDeleter, name string) v1.RevokeNodeResponse {
	t.Helper()
	srv := newTestServer(t, hs, s)
	defer srv.Close()

	resp := deleteReq(t, srv.URL+"/api/v1/overlay/nodes/"+name)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out v1.RevokeNodeResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func deleteReq(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
