// Package headscale is a thin client for the Headscale management HTTP API
// (API v1). cara-server uses it to issue pre-auth keys and to list and delete
// overlay nodes on behalf of caractl, so operators never shell into the
// Headscale container.
//
// Field shapes target Headscale v0.29.x's /api/v1 endpoints. The pre-auth key
// and the API key are secret material and are never logged.
package headscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Client is the subset of Headscale management operations cara-server needs.
// It is an interface so the server handler can be unit-tested with a fake.
type Client interface {
	// CreatePreAuthKey issues a new pre-auth key under the given user.
	CreatePreAuthKey(ctx context.Context, req CreatePreAuthKeyRequest) (PreAuthKey, error)
	// ListNodes returns every node currently known to Headscale.
	ListNodes(ctx context.Context) ([]Node, error)
	// DeleteNode removes a node by its Headscale ID. Deleting a node that no
	// longer exists is treated as success, so the operation is idempotent.
	DeleteNode(ctx context.Context, id string) error
}

// CreatePreAuthKeyRequest describes a pre-auth key to issue.
type CreatePreAuthKeyRequest struct {
	// User is the Headscale user the key is created under.
	User string
	// Reusable false means the key is single-use (one node join).
	Reusable bool
	// Ephemeral false means the node persists after it disconnects.
	Ephemeral bool
	// Expiration is when the key stops being valid.
	Expiration time.Time
}

// PreAuthKey is a freshly issued Headscale pre-auth key. Key is secret.
type PreAuthKey struct {
	Key        string
	Expiration time.Time
}

// Node is a Headscale overlay node.
type Node struct {
	ID          string
	Name        string
	IPAddresses []string
	Online      bool
}

// HTTPClient talks to the Headscale management API over HTTP with a Bearer
// API key.
type HTTPClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	logger  *zap.Logger
}

// NewHTTPClient returns a Headscale client pointed at baseURL (e.g.
// "http://localhost:8081"), authenticating with the given API key.
func NewHTTPClient(baseURL, apiKey string, logger *zap.Logger) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
		logger:  logger,
	}
}

// wire types mirror the Headscale API v1 JSON shapes.

type wirePreAuthKey struct {
	Key        string    `json:"key"`
	Expiration time.Time `json:"expiration"`
}

type createPreAuthKeyRequestBody struct {
	User       string `json:"user"`
	Reusable   bool   `json:"reusable"`
	Ephemeral  bool   `json:"ephemeral"`
	Expiration string `json:"expiration,omitempty"`
}

type createPreAuthKeyResponse struct {
	PreAuthKey wirePreAuthKey `json:"preAuthKey"`
}

type wireNode struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	GivenName   string   `json:"givenName"`
	IPAddresses []string `json:"ipAddresses"`
	Online      bool     `json:"online"`
}

type listNodesResponse struct {
	Nodes []wireNode `json:"nodes"`
}

// CreatePreAuthKey implements Client.
func (c *HTTPClient) CreatePreAuthKey(ctx context.Context, req CreatePreAuthKeyRequest) (PreAuthKey, error) {
	body := createPreAuthKeyRequestBody{
		User:      req.User,
		Reusable:  req.Reusable,
		Ephemeral: req.Ephemeral,
	}
	if !req.Expiration.IsZero() {
		body.Expiration = req.Expiration.UTC().Format(time.RFC3339)
	}

	var out createPreAuthKeyResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/preauthkey", body, &out); err != nil {
		return PreAuthKey{}, err
	}
	return PreAuthKey{Key: out.PreAuthKey.Key, Expiration: out.PreAuthKey.Expiration}, nil
}

// ListNodes implements Client.
func (c *HTTPClient) ListNodes(ctx context.Context) ([]Node, error) {
	var out listNodesResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/node", nil, &out); err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		name := n.GivenName
		if name == "" {
			name = n.Name
		}
		nodes = append(nodes, Node{
			ID:          n.ID,
			Name:        name,
			IPAddresses: n.IPAddresses,
			Online:      n.Online,
		})
	}
	return nodes, nil
}

// DeleteNode implements Client. A 404 is treated as success so revoke is
// idempotent: re-running against an already-removed node is not an error.
func (c *HTTPClient) DeleteNode(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/node/"+id, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// do performs an authenticated JSON request. When out is non-nil the response
// body is decoded into it. Request and response bodies are never logged because
// they may carry pre-auth keys.
func (c *HTTPClient) do(ctx context.Context, method, path string, in, out any) error {
	var reqBody io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("headscale: encode request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("headscale: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("headscale: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a bounded amount of the error body for context. Headscale error
		// bodies do not contain the created key.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &apiError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("headscale: decode response: %w", err)
	}
	return nil
}

// apiError is a non-2xx response from Headscale.
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("headscale: unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("headscale: unexpected status %d: %s", e.StatusCode, e.Body)
}

// isNotFound reports whether err is a Headscale 404 response.
func isNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
