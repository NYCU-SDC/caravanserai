//go:build e2e

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issueToken asks the server for a token as the break-glass admin and returns
// it.
func issueToken(t *testing.T, kind v1.APITokenKind, name string) string {
	t.Helper()
	created := issueTokenRecord(t, kind, name)
	return created.Token
}

func issueTokenRecord(t *testing.T, kind v1.APITokenKind, name string) v1.APITokenCreated {
	t.Helper()
	resp := doRequest(t, http.MethodPost, "/api/v1/tokens", mustMarshal(t, v1.APITokenRequest{Kind: kind, Name: name}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created v1.APITokenCreated
	mustDecodeBody(t, resp, &created)
	require.NotEmpty(t, created.Token)
	return created
}

// requestAs sends a request carrying exactly the given Authorization header;
// "" sends none.
func requestAs(t *testing.T, authorization, method, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, suite.serverURL+path, bytes.NewReader(body))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// A plain client: suite.client would add the admin token.
	client := &http.Client{}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// TestAuthRequiredOnEveryRoute: without a valid token nothing but the health
// probe answers, whatever the route.
func TestAuthRequiredOnEveryRoute(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nodes"},
		{http.MethodPost, "/api/v1/nodes"},
		{http.MethodPost, "/api/v1/nodes/node-a/heartbeat"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodPost, "/api/v1/projects"},
		{http.MethodDelete, "/api/v1/projects/web"},
		{http.MethodPatch, "/api/v1/projects/web/status"},
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodGet, "/api/v1/secrets/db"},
		{http.MethodPost, "/api/v1/tokens"},
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodGet, "/api/v1/no-such-route"},
	}
	for _, header := range []string{"", "Bearer ", "Bearer wrong", "Basic " + testAdminToken} {
		for _, rt := range routes {
			resp := requestAs(t, header, rt.method, rt.path, []byte("{}"))
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s %s with %q", rt.method, rt.path, header)
			drainBody(resp)
		}
	}

	resp := requestAs(t, "", http.MethodGet, "/api/healthz", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)
}

// TestAPITokenLifecycle: an issued token works until it is revoked, and the
// server keeps only its hash.
func TestAPITokenLifecycle(t *testing.T) {
	created := issueTokenRecord(t, v1.APITokenKindUser, "lifecycle-user")
	assert.Equal(t, "user:break-glass", created.CreatedBy)

	bearer := "Bearer " + created.Token
	resp := requestAs(t, bearer, http.MethodGet, "/api/v1/nodes", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	drainBody(resp)

	stored, err := suite.store.LookupAPIToken(context.Background(), auth.HashToken(created.Token))
	require.NoError(t, err, "the token is found by its hash")
	assert.Equal(t, created.ID, stored.ID)
	_, err = suite.store.LookupAPIToken(context.Background(), created.Token)
	assert.ErrorIs(t, err, store.ErrNotFound, "the token itself is not what is stored")

	resp = doRequest(t, http.MethodGet, "/api/v1/tokens", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rawBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	drainBody(resp)
	raw := string(rawBytes)
	assert.NotContains(t, raw, created.Token, "listing must not reveal tokens")
	assert.Contains(t, raw, "lifecycle-user")

	resp = doRequest(t, http.MethodDelete, "/api/v1/tokens/"+strconv.FormatInt(created.ID, 10), nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	drainBody(resp)

	resp = requestAs(t, bearer, http.MethodGet, "/api/v1/nodes", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a revoked token is refused at once")
	drainBody(resp)

	resp = doRequest(t, http.MethodDelete, "/api/v1/tokens/"+strconv.FormatInt(created.ID, 10), nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "revoking twice is a no-op")
	drainBody(resp)

	resp = doRequest(t, http.MethodDelete, "/api/v1/tokens/999999", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	drainBody(resp)
}

// TestNodeTokenCannotManageTokens: a Node must not be able to mint or revoke an
// identity.
func TestNodeTokenCannotManageTokens(t *testing.T) {
	bearer := "Bearer " + issueToken(t, v1.APITokenKindNode, "token-node")
	body := mustMarshal(t, v1.APITokenRequest{Kind: v1.APITokenKindUser, Name: "escalated"})
	for _, rt := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPost, "/api/v1/tokens", body},
		{http.MethodGet, "/api/v1/tokens", nil},
		{http.MethodDelete, "/api/v1/tokens/1", nil},
	} {
		resp := requestAs(t, bearer, rt.method, rt.path, rt.body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", rt.method, rt.path)
		drainBody(resp)
	}
}

// TestTokenRequestValidation: unknown kinds, bad names and the reserved
// break-glass name are refused.
func TestTokenRequestValidation(t *testing.T) {
	for _, req := range []v1.APITokenRequest{
		{Kind: "robot", Name: "x"},
		{Kind: v1.APITokenKindNode, Name: "Not_A_Name"},
		{Kind: v1.APITokenKindUser, Name: auth.BreakGlassName},
	} {
		resp := doRequest(t, http.MethodPost, "/api/v1/tokens", mustMarshal(t, req))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%+v", req)
		drainBody(resp)
	}
}
