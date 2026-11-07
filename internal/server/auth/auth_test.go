package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeTokens maps hashes to tokens, the way the store looks them up.
type fakeTokens struct {
	byHash map[string]v1.APIToken
	err    error
}

func (f fakeTokens) LookupAPIToken(_ context.Context, hash string) (v1.APIToken, error) {
	if f.err != nil {
		return v1.APIToken{}, f.err
	}
	t, ok := f.byHash[hash]
	if !ok {
		return v1.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func TestNewTokenIsRandomAndPrefixed(t *testing.T) {
	a, err := NewToken()
	require.NoError(t, err)
	b, err := NewToken()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(a, tokenPrefix))
	assert.NotEqual(t, a, b)
	assert.Len(t, HashToken(a), 64)
	assert.NotContains(t, HashToken(a), a)
}

func TestAuthenticate(t *testing.T) {
	nodeToken, userToken := "cara_node", "cara_user"
	tokens := fakeTokens{byHash: map[string]v1.APIToken{
		HashToken(nodeToken): {Kind: v1.APITokenKindNode, Name: "node-a"},
		HashToken(userToken): {Kind: v1.APITokenKindUser, Name: "hank"},
	}}
	a := NewAuthenticator(zap.NewNop(), tokens, "glass")

	cases := []struct {
		token string
		want  Principal
		err   error
	}{
		{nodeToken, Principal{Kind: v1.APITokenKindNode, Name: "node-a"}, nil},
		{userToken, Principal{Kind: v1.APITokenKindUser, Name: "hank"}, nil},
		{"glass", Principal{Kind: v1.APITokenKindUser, Name: BreakGlassName}, nil},
		{"", Principal{}, ErrUnauthenticated},
		{"cara_unknown", Principal{}, ErrUnauthenticated},
	}
	for _, tc := range cases {
		got, err := a.Authenticate(context.Background(), tc.token)
		assert.ErrorIs(t, err, tc.err, "token %q", tc.token)
		assert.Equal(t, tc.want, got, "token %q", tc.token)
	}
}

// With no break-glass token configured, an empty token must not match it.
func TestAuthenticateWithoutBreakGlass(t *testing.T) {
	a := NewAuthenticator(zap.NewNop(), fakeTokens{}, "")
	for _, token := range []string{"", " "} {
		_, err := a.Authenticate(context.Background(), token)
		assert.ErrorIs(t, err, ErrUnauthenticated)
	}
}

func TestMiddleware(t *testing.T) {
	tokens := fakeTokens{byHash: map[string]v1.APIToken{
		HashToken("cara_node"): {Kind: v1.APITokenKindNode, Name: "node-a"},
	}}
	var seen Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := NewAuthenticator(zap.NewNop(), tokens, "").Middleware(next)

	serve := func(path, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := serve("/api/v1/projects", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Bearer realm="cara"`, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))

	assert.Equal(t, http.StatusUnauthorized, serve("/api/v1/projects", "Bearer cara_unknown").Code)
	assert.Equal(t, http.StatusUnauthorized, serve("/api/v1/projects", "Basic cara_node").Code)
	assert.Equal(t, http.StatusUnauthorized, serve("/api/v1/projects", "cara_node").Code, "the scheme is required")

	rec = serve("/api/v1/projects", "Bearer cara_node")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, Principal{Kind: v1.APITokenKindNode, Name: "node-a"}, seen)

	seen = Principal{}
	assert.Equal(t, http.StatusNoContent, serve("/api/healthz", "").Code)
	assert.Equal(t, Principal{}, seen, "the health probe carries no principal")
}

// A store failure is the server's fault, not the caller's: 500, not 401.
func TestMiddlewareStoreFailure(t *testing.T) {
	h := NewAuthenticator(zap.NewNop(), fakeTokens{err: errors.New("db down")}, "").
		Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer cara_x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestPrincipalIsAdmin(t *testing.T) {
	assert.True(t, Principal{Kind: v1.APITokenKindUser, Name: "hank"}.IsAdmin())
	assert.False(t, Principal{Kind: v1.APITokenKindNode, Name: "node-a"}.IsAdmin())
	assert.Equal(t, "node:node-a", Principal{Kind: v1.APITokenKindNode, Name: "node-a"}.String())
}

// The join route is the one place a request without an API token gets through:
// the join token is checked by its handler. Nothing near it may slip through.
func TestMiddlewareLetsOnlyTheJoinRouteThrough(t *testing.T) {
	reached := false
	h := NewAuthenticator(zap.NewNop(), fakeTokens{}, "").Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, ok := FromContext(r.Context())
		assert.False(t, ok, "a join request carries no principal")
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		method, path string
		through      bool
	}{
		{http.MethodPost, "/api/v1/nodes/node-a/join", true},
		{http.MethodGet, "/api/v1/nodes/node-a/join", false},
		{http.MethodPost, "/api/v1/nodes//join", false},
		{http.MethodPost, "/api/v1/nodes/a/b/join", false},
		{http.MethodPost, "/api/v1/nodes/node-a/join-tokens", false},
		{http.MethodPost, "/api/v1/nodes/node-a/joined", false},
		{http.MethodPost, "/api/v1/nodes", false},
	}
	for _, tc := range cases {
		reached = false
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer cara_join")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, tc.through, reached, "%s %s", tc.method, tc.path)
		if !tc.through {
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", tc.method, tc.path)
		}
	}
}
