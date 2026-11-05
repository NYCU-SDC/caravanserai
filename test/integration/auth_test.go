//go:build e2e

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

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
		{http.MethodPost, "/api/v1/nodes/node-a/join-tokens"},
		{http.MethodDelete, "/api/v1/nodes/node-a/tokens"},
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

// issueJoinToken issues a join token for node as the break-glass admin.
func issueJoinToken(t *testing.T, node, ttl string) v1.NodeJoinTokenCreated {
	t.Helper()
	resp := doRequest(t, http.MethodPost, "/api/v1/nodes/"+node+"/join-tokens", mustMarshal(t, v1.NodeJoinTokenRequest{TTL: ttl}))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created v1.NodeJoinTokenCreated
	mustDecodeBody(t, resp, &created)
	require.NotEmpty(t, created.Token)
	return created
}

// join presents joinToken at node's join route and returns the response.
func join(t *testing.T, node, joinToken string) *http.Response {
	t.Helper()
	authorization := ""
	if joinToken != "" {
		authorization = "Bearer " + joinToken
	}
	return requestAs(t, authorization, http.MethodPost, "/api/v1/nodes/"+node+"/join", nil)
}

func registerAs(t *testing.T, bearer, node string) *http.Response {
	t.Helper()
	body := mustMarshal(t, v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: node},
		Spec:       v1.NodeSpec{Hostname: node},
	})
	return requestAs(t, bearer, http.MethodPost, "/api/v1/nodes", body)
}

// TestNodeJoin: a join token enrols one Node, once, and the Node token it
// becomes registers and acts as that Node only.
func TestNodeJoin(t *testing.T) {
	created := issueJoinToken(t, "join-a", "")
	assert.Equal(t, "join-a", created.NodeName)
	assert.Equal(t, "user:break-glass", created.CreatedBy)
	assert.WithinDuration(t, created.CreatedAt.Add(time.Hour), created.ExpiresAt, time.Minute, "the default TTL is an hour")

	resp := requestAs(t, "Bearer "+created.Token, http.MethodGet, "/api/v1/nodes", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a join token is not an API token")
	drainBody(resp)

	resp = join(t, "join-a", created.Token)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var joined v1.NodeJoined
	mustDecodeBody(t, resp, &joined)
	assert.Equal(t, v1.APITokenKindNode, joined.Kind)
	assert.Equal(t, "join-a", joined.Name)
	assert.Equal(t, "join-token:"+strconv.FormatInt(created.ID, 10), joined.CreatedBy)
	nodeBearer := "Bearer " + joined.Token

	resp = join(t, "join-a", created.Token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a join token works once")
	drainBody(resp)

	resp = registerAs(t, nodeBearer, "join-other")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a Node registers only its own name")
	drainBody(resp)

	resp = registerAs(t, nodeBearer, "join-a")
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	drainBody(resp)
	t.Cleanup(func() { drainBody(doRequest(t, http.MethodDelete, "/api/v1/nodes/join-a", nil)) })

	resp = requestAs(t, nodeBearer, http.MethodPost, "/api/v1/nodes/join-a/heartbeat", []byte("{}"))
	assert.Less(t, resp.StatusCode, 300, "the Node token is accepted for the Node's own calls")
	drainBody(resp)
}

// TestNodeJoinRefusals: every way a join token can be wrong is refused, and a
// join for the wrong Node does not use the token up.
func TestNodeJoinRefusals(t *testing.T) {
	for _, header := range []string{"", "cara_unknown"} {
		resp := join(t, "join-r", header)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "join token %q", header)
		drainBody(resp)
	}
	resp := requestAs(t, "Basic x", http.MethodPost, "/api/v1/nodes/join-r/join", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	drainBody(resp)

	forR := issueJoinToken(t, "join-r", "")
	resp = join(t, "join-s", forR.Token)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a join token enrols only the Node it names")
	drainBody(resp)
	resp = join(t, "join-r", forR.Token)
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "the refused attempt did not use the token")
	drainBody(resp)

	short := issueJoinToken(t, "join-r", "1s")
	time.Sleep(1100 * time.Millisecond)
	resp = join(t, "join-r", short.Token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "an expired join token is refused")
	drainBody(resp)

	for _, ttl := range []string{"0s", "-1h", "169h", "soon"} {
		resp := doRequest(t, http.MethodPost, "/api/v1/nodes/join-r/join-tokens", mustMarshal(t, v1.NodeJoinTokenRequest{TTL: ttl}))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "ttl %q", ttl)
		drainBody(resp)
	}
}

// TestNodeRevoke: revoking a Node stops its Node token and its unused join
// tokens at once, and a new join token brings it back.
func TestNodeRevoke(t *testing.T) {
	first := issueJoinToken(t, "join-v", "")
	resp := join(t, "join-v", first.Token)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var joined v1.NodeJoined
	mustDecodeBody(t, resp, &joined)
	pending := issueJoinToken(t, "join-v", "")

	resp = doRequest(t, http.MethodDelete, "/api/v1/nodes/join-v/tokens", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var revoked v1.NodeTokensRevoked
	mustDecodeBody(t, resp, &revoked)
	assert.Equal(t, v1.NodeTokensRevoked{NodeTokens: 1, JoinTokens: 1}, revoked)

	resp = requestAs(t, "Bearer "+joined.Token, http.MethodGet, "/api/v1/nodes", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the revoked Node token is refused")
	drainBody(resp)
	resp = join(t, "join-v", pending.Token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the unused join token is revoked too")
	drainBody(resp)

	again := issueJoinToken(t, "join-v", "")
	resp = join(t, "join-v", again.Token)
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "a new join token enrols the Node again")
	drainBody(resp)

	resp = doRequest(t, http.MethodDelete, "/api/v1/nodes/no-such-node/tokens", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "revoking a Node with no tokens is not an error")
	drainBody(resp)
}

// TestNodeTokenCannotEnrolNodes: a Node must not mint join tokens or revoke
// another Node.
func TestNodeTokenCannotEnrolNodes(t *testing.T) {
	bearer := "Bearer " + issueToken(t, v1.APITokenKindNode, "join-n")
	resp := requestAs(t, bearer, http.MethodPost, "/api/v1/nodes/join-n/join-tokens", []byte("{}"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	drainBody(resp)
	resp = requestAs(t, bearer, http.MethodDelete, "/api/v1/nodes/join-a/tokens", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	drainBody(resp)
}

// TestNodeJoinOnceUnderRace: a join token presented many times at once is
// redeemed once.
func TestNodeJoinOnceUnderRace(t *testing.T) {
	created := issueJoinToken(t, "join-race", "")
	const tries = 10
	codes := make(chan int, tries)
	var wg sync.WaitGroup
	for range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, suite.serverURL+"/api/v1/nodes/join-race/join", nil)
			if err != nil {
				codes <- 0
				return
			}
			req.Header.Set("Authorization", "Bearer "+created.Token)
			resp, err := (&http.Client{}).Do(req)
			if err != nil {
				codes <- 0
				return
			}
			drainBody(resp)
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	assert.Equal(t, map[int]int{http.StatusCreated: 1, http.StatusUnauthorized: tries - 1}, counts)
}
