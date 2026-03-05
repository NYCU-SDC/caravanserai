// Package agent implements the cara-agent control-plane client.
//
// The Client talks to the cara-server REST API to:
//   - Register this node
//   - Send periodic heartbeats
//   - Poll for Scheduled and Terminating Projects assigned to this node
//   - Report project status (Running / Failed / Terminated) back to the server
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// Client is an HTTP client for the cara-server node API.
type Client struct {
	serverURL  string
	nodeName   string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewClient creates a Client that will identify itself as nodeName and dial
// serverURL (e.g. "http://cara-server:8080").
func NewClient(logger *zap.Logger, serverURL, nodeName string) *Client {
	return &Client{
		serverURL: serverURL,
		nodeName:  nodeName,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger: logger,
	}
}

// Register calls POST /api/v1/nodes to self-register the node.
// If the node already exists (HTTP 409) the call is treated as a no-op.
func (c *Client) Register(ctx context.Context, spec v1.NodeSpec) error {
	node := v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: c.nodeName},
		Spec:       spec,
	}

	body, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("marshal register request: %w", err)
	}

	url := c.serverURL + "/api/v1/nodes"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		c.logger.Info("Node registered", zap.String("node", c.nodeName))
		return nil
	case http.StatusConflict:
		c.logger.Info("Node already registered, continuing", zap.String("node", c.nodeName))
		return nil
	default:
		return fmt.Errorf("register: unexpected status %s", resp.Status)
	}
}

// heartbeatRequest mirrors the server-side type; only the fields the agent
// cares about are included.
type heartbeatRequest struct {
	State       v1.NodeState         `json:"state,omitempty"`
	Network     v1.NodeNetworkStatus `json:"network,omitempty"`
	Capacity    v1.ResourceList      `json:"capacity,omitempty"`
	Allocatable v1.ResourceList      `json:"allocatable,omitempty"`
}

// Heartbeat calls POST /api/v1/nodes/{name}/heartbeat.
// Passing an empty NodeStatus is valid — the server will update only the
// LastHeartbeat timestamp.
func (c *Client) Heartbeat(ctx context.Context, status v1.NodeStatus) error {
	req := heartbeatRequest{
		State:       status.State,
		Network:     status.Network,
		Capacity:    status.Capacity,
		Allocatable: status.Allocatable,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal heartbeat request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/nodes/%s/heartbeat", c.serverURL, c.nodeName)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build heartbeat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("heartbeat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("heartbeat: unexpected status %s", resp.Status)
	}

	c.logger.Debug("Heartbeat sent", zap.String("node", c.nodeName))
	return nil
}

// ListScheduledProjects calls GET /api/v1/projects?phase=Scheduled&nodeRef=<nodeName>
// and returns the projects the server has scheduled onto this node.
// The Agent reconcile loop calls this on each poll tick.
func (c *Client) ListScheduledProjects(ctx context.Context) ([]*v1.Project, error) {
	url := fmt.Sprintf("%s/api/v1/projects?phase=Scheduled&nodeRef=%s", c.serverURL, c.nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build list projects request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list projects: unexpected status %s", resp.Status)
	}

	var list v1.ProjectList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode project list: %w", err)
	}

	projects := make([]*v1.Project, len(list.Items))
	for i := range list.Items {
		projects[i] = &list.Items[i]
	}

	return projects, nil
}

// ListProjectsForReconcile calls GET /api/v1/projects with phase=Scheduled and
// phase=Terminating filters, restricted to this node via nodeRef.  The result
// includes both projects that need to be started and those that need to be torn
// down.
func (c *Client) ListProjectsForReconcile(ctx context.Context) ([]*v1.Project, error) {
	url := fmt.Sprintf("%s/api/v1/projects?phase=Scheduled&phase=Terminating&nodeRef=%s",
		c.serverURL, c.nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build list projects request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list projects: unexpected status %s", resp.Status)
	}

	var list v1.ProjectList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode project list: %w", err)
	}

	projects := make([]*v1.Project, len(list.Items))
	for i := range list.Items {
		projects[i] = &list.Items[i]
	}

	return projects, nil
}

// projectStatusRequest is the body sent to PATCH /api/v1/projects/{name}/status.
type projectStatusRequest struct {
	Phase   v1.ProjectPhase `json:"phase"`
	Reason  string          `json:"reason,omitempty"`
	Message string          `json:"message,omitempty"`
}

// UpdateProjectStatus calls PATCH /api/v1/projects/{name}/status to report the
// observed phase to the server.  phase should be Running or Failed.
func (c *Client) UpdateProjectStatus(ctx context.Context, projectName string, phase v1.ProjectPhase, reason, message string) error {
	reqBody := projectStatusRequest{
		Phase:   phase,
		Reason:  reason,
		Message: message,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal project status request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/projects/%s/status", c.serverURL, projectName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build project status request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("project status request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("update project status: unexpected status %s", resp.Status)
	}

	c.logger.Info("Project status updated",
		zap.String("project", projectName),
		zap.String("phase", string(phase)),
	)
	return nil
}
