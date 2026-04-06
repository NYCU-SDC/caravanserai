package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestServer_ProxiesToBackend(t *testing.T) {
	// Start a fake backend that returns a known response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "proxied")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from backend"))
	}))
	defer backend.Close()

	// Parse backend URL to get host:port.
	backendHost := strings.TrimPrefix(backend.URL, "http://")

	rt := NewRouteTable(zap.NewNop())

	// Manually set the route to point to our test backend.
	rt.mu.Lock()
	rt.routes["app.example.com"] = backend.URL
	rt.projectRoutes["test"] = []string{"app.example.com"}
	rt.mu.Unlock()

	_ = backendHost

	srv := NewServer(zap.NewNop(), ":0", rt)

	// Use httptest to test the handler directly.
	req := httptest.NewRequest("GET", "/some/path", nil)
	req.Host = "app.example.com"
	rec := httptest.NewRecorder()

	srv.handler().ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello from backend", string(body))
	assert.Equal(t, "proxied", resp.Header.Get("X-Test"))
}

func TestServer_UnknownHostReturns502(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())
	srv := NewServer(zap.NewNop(), ":0", rt)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "unknown.example.com"
	rec := httptest.NewRecorder()

	srv.handler().ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	// Verify the error page content.
	assert.Contains(t, bodyStr, "502")
	assert.Contains(t, bodyStr, "Bad Gateway")
	assert.Contains(t, bodyStr, "unknown.example.com")
	assert.Contains(t, bodyStr, "Caravanserai Proxy")
}

func TestServer_ForwardsHeaders(t *testing.T) {
	var receivedHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rt := NewRouteTable(zap.NewNop())
	rt.mu.Lock()
	rt.routes["test.example.com"] = backend.URL
	rt.projectRoutes["test"] = []string{"test.example.com"}
	rt.mu.Unlock()

	srv := NewServer(zap.NewNop(), ":0", rt)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "test.example.com"
	req.Header.Set("X-Custom", "custom-value")
	rec := httptest.NewRecorder()

	srv.handler().ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify X-Forwarded-* headers are set.
	assert.NotEmpty(t, receivedHeaders.Get("X-Forwarded-For"))
	assert.Equal(t, "test.example.com", receivedHeaders.Get("X-Forwarded-Host"))

	// Custom headers should be forwarded.
	assert.Equal(t, "custom-value", receivedHeaders.Get("X-Custom"))
}

func TestServer_HostWithPort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	rt := NewRouteTable(zap.NewNop())
	rt.mu.Lock()
	rt.routes["app.example.com"] = backend.URL
	rt.projectRoutes["test"] = []string{"app.example.com"}
	rt.mu.Unlock()

	srv := NewServer(zap.NewNop(), ":0", rt)

	// Host header includes a port — Lookup should strip it.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.example.com:8081"
	rec := httptest.NewRecorder()

	srv.handler().ServeHTTP(rec, req)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
