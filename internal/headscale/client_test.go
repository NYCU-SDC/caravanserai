package headscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCreatePreAuthKey(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"preAuthKey":{"key":"secret-key-123","expiration":"2026-08-20T00:00:00Z"}}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "api-token", zap.NewNop())
	key, err := c.CreatePreAuthKey(context.Background(), CreatePreAuthKeyRequest{
		User:       "cara-node",
		Expiration: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.Equal(t, "secret-key-123", key.Key)
	assert.Equal(t, "Bearer api-token", gotAuth)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/preauthkey", gotPath)
}

func TestListNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/node", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[
			{"id":"1","name":"raw-name","givenName":"agent-a","ipAddresses":["100.64.0.1"],"online":true},
			{"id":"2","name":"agent-b","givenName":"","ipAddresses":["100.64.0.2"],"online":false}
		]}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "api-token", zap.NewNop())
	nodes, err := c.ListNodes(context.Background())
	require.NoError(t, err)

	require.Len(t, nodes, 2)
	// givenName is preferred as the display name.
	assert.Equal(t, "agent-a", nodes[0].Name)
	assert.Equal(t, "1", nodes[0].ID)
	assert.Equal(t, []string{"100.64.0.1"}, nodes[0].IPAddresses)
	assert.True(t, nodes[0].Online)
	// falls back to name when givenName is empty.
	assert.Equal(t, "agent-b", nodes[1].Name)
}

func TestDeleteNode(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "200 deletes", statusCode: http.StatusOK, wantErr: false},
		{name: "404 is treated as success (idempotent)", statusCode: http.StatusNotFound, wantErr: false},
		{name: "500 is an error", statusCode: http.StatusInternalServerError, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodDelete, r.Method)
				assert.Equal(t, "/api/v1/node/7", r.URL.Path)
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			c := NewHTTPClient(srv.URL, "api-token", zap.NewNop())
			err := c.DeleteNode(context.Background(), "7")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
