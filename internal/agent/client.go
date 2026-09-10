// Package agent implements the cara-agent control-plane client.
//
// The Client talks to the cara-server REST API to:
//   - Register this node
//   - Send periodic heartbeats
//   - Poll for Scheduled, Running, and Terminating Projects assigned to this node
//   - Report project status (Running / Failed / Terminated) back to the server
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// ErrNodeNotFound is returned by Heartbeat when the server responds with 404,
// indicating the node no longer exists and should re-register.
var ErrNodeNotFound = errors.New("node not found")

// ErrStaleAssignment is returned by the fenced status and condition writes when
// the server rejects the report with 409 Conflict (CARA-83). The server maps
// both a stale assignment fence and a lost optimistic-concurrency race to 409,
// and the Agent's correct response to either is identical: this view of the
// Project is no longer authoritative, so it must abandon the report and wait
// for the next ownership poll to re-derive the current UID, nodeRef, and
// generation rather than retrying the same stale data.
var ErrStaleAssignment = errors.New("stale assignment: report rejected by server")

// AssignmentFence is the ownership identity an Agent presents on every write:
// the Project UID it was told to run, the Node it believes it is, and the
// assignment generation that authorised it. The server validates all three
// atomically against the Project's current ownership.
type AssignmentFence struct {
	UID        string
	NodeRef    string
	Generation int64
}

// fenceForProject reads the assignment fence from a Project the Agent received
// in an ownership poll. The three fields are exactly what the Scheduler wrote
// when it granted this assignment, so echoing them back is what proves the
// Agent is acting on the current grant and not a stale one.
func fenceForProject(p *v1.Project) AssignmentFence {
	return AssignmentFence{
		UID:        p.ObjectMeta.UID,
		NodeRef:    p.Status.NodeRef,
		Generation: p.Status.AssignmentGeneration,
	}
}

// Client is an HTTP client for the cara-server node API.
type Client struct {
	serverURL  string
	nodeName   string
	httpClient *http.Client
	logger     *zap.Logger

	// overlayIP is the Headscale-assigned overlay IP set after the agent
	// joins the overlay network.  Empty when overlay networking is disabled.
	overlayIP string

	// keyRef is the hex SHA-256 of the pre-auth key this agent joined with.
	// Sent on heartbeats so the server can bind this node to the key it was
	// issued for (CARA-68).  Empty when overlay networking is disabled.
	keyRef string
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

// SetOverlayIP records the overlay IP assigned to this agent after joining the
// Headscale overlay network.
func (c *Client) SetOverlayIP(ip string) {
	c.overlayIP = ip
}

// OverlayIP returns the overlay IP recorded by SetOverlayIP, or the empty
// string when overlay networking is disabled.
func (c *Client) OverlayIP() string {
	return c.overlayIP
}

// SetKeyRef records the pre-auth key reference (a hash, never the key itself)
// the agent joined the overlay with, so heartbeats can carry it for server-side
// identity binding (CARA-68).
func (c *Client) SetKeyRef(ref string) {
	c.keyRef = ref
}

// Register calls POST /api/v1/nodes to self-register the node.
// If the node already exists (HTTP 409) the call is treated as a no-op.
func (c *Client) Register(ctx context.Context, spec v1.NodeSpec) error {
	node := v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: c.nodeName},
		Spec:       spec,
		Status: v1.NodeStatus{
			Network: v1.NodeNetworkStatus{OverlayIP: c.overlayIP},
		},
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
		if c.overlayIP != "" {
			if err := c.Heartbeat(ctx, v1.NodeStatus{
				Network: v1.NodeNetworkStatus{OverlayIP: c.overlayIP},
			}); err != nil {
				return fmt.Errorf("refresh overlay address after register conflict: %w", err)
			}
		}
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
	KeyRef      string               `json:"keyRef,omitempty"`
}

// Heartbeat calls POST /api/v1/nodes/{name}/heartbeat.
// Passing an empty NodeStatus is valid — the server will update only the
// LastHeartbeat timestamp.
func (c *Client) Heartbeat(ctx context.Context, status v1.NodeStatus) error {
	network := status.Network
	if network.OverlayIP == "" {
		network.OverlayIP = c.overlayIP
	}

	req := heartbeatRequest{
		State:       status.State,
		Network:     network,
		Capacity:    status.Capacity,
		Allocatable: status.Allocatable,
		KeyRef:      c.keyRef,
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

	switch resp.StatusCode {
	case http.StatusNoContent:
		c.logger.Debug("Heartbeat sent", zap.String("node", c.nodeName))
		return nil
	case http.StatusNotFound:
		return ErrNodeNotFound
	default:
		return fmt.Errorf("heartbeat: unexpected status %s", resp.Status)
	}
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

// ListProjectsAssignedToNode returns the complete ownership snapshot for this
// node. It intentionally applies no phase filter: Failed and future phases are
// still owned when status.nodeRef points here, even if they need no reconcile
// action in this tick.
func (c *Client) ListProjectsAssignedToNode(ctx context.Context) ([]*v1.Project, error) {
	url := fmt.Sprintf("%s/api/v1/projects?nodeRef=%s", c.serverURL, c.nodeName)
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

// ErrProjectNotFound is returned by GetProject when the server responds 404,
// meaning the Project no longer exists at all.
var ErrProjectNotFound = errors.New("project not found")

// GetProject fetches a single Project by name. It returns ErrProjectNotFound
// if the server responds 404, which callers must distinguish from a transport
// failure: "deleted" and "unreachable" lead to opposite decisions when a
// backup is deciding whether to restart containers.
func (c *Client) GetProject(ctx context.Context, name string) (*v1.Project, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%s", c.serverURL, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build get project request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get project request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrProjectNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get project: unexpected status %s", resp.Status)
	}

	var project v1.Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return nil, fmt.Errorf("decode project: %w", err)
	}
	return &project, nil
}

// conditionPatchRequest is the body sent to
// PATCH /api/v1/projects/{name}/conditions/{type}. The uid/nodeRef/generation
// triple is the assignment fence the server validates atomically.
type conditionPatchRequest struct {
	Status               v1.ConditionStatus `json:"status"`
	Reason               string             `json:"reason,omitempty"`
	Message              string             `json:"message,omitempty"`
	UID                  string             `json:"uid,omitempty"`
	NodeRef              string             `json:"nodeRef,omitempty"`
	AssignmentGeneration *int64             `json:"assignmentGeneration,omitempty"`
}

// PatchProjectCondition sets one condition on a Project without touching its
// phase. Used to advertise Maintenance during a backup: the Project must stay
// Running throughout, so the backup cannot report itself by moving the phase.
// The fence is presented so the server can reject the write if this Agent no
// longer owns the assignment; a 409 comes back as ErrStaleAssignment.
func (c *Client) PatchProjectCondition(ctx context.Context, projectName string, fence AssignmentFence, condType v1.ConditionType, status v1.ConditionStatus, reason, message string) error {
	gen := fence.Generation
	body, err := json.Marshal(conditionPatchRequest{
		Status: status, Reason: reason, Message: message,
		UID: fence.UID, NodeRef: fence.NodeRef, AssignmentGeneration: &gen,
	})
	if err != nil {
		return fmt.Errorf("marshal condition patch request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/projects/%s/conditions/%s", c.serverURL, projectName, condType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build condition patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("condition patch request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusConflict {
		return ErrStaleAssignment
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("patch condition: unexpected status %s", resp.Status)
	}
	return nil
}

// ClearProjectCondition removes one condition from a Project. The fence travels
// as query parameters because a DELETE carries no body.
func (c *Client) ClearProjectCondition(ctx context.Context, projectName string, fence AssignmentFence, condType v1.ConditionType) error {
	url := fmt.Sprintf("%s/api/v1/projects/%s/conditions/%s", c.serverURL, projectName, condType)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build condition delete request: %w", err)
	}
	q := req.URL.Query()
	q.Set("uid", fence.UID)
	q.Set("nodeRef", fence.NodeRef)
	q.Set("assignmentGeneration", strconv.FormatInt(fence.Generation, 10))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("condition delete request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusConflict {
		return ErrStaleAssignment
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("clear condition: unexpected status %s", resp.Status)
	}
	return nil
}

// projectStatusRequest is the body sent to PATCH /api/v1/projects/{name}/status.
type projectStatusRequest struct {
	Phase                v1.ProjectPhase `json:"phase"`
	Reason               string          `json:"reason,omitempty"`
	Message              string          `json:"message,omitempty"`
	UID                  string          `json:"uid,omitempty"`
	NodeRef              string          `json:"nodeRef,omitempty"`
	AssignmentGeneration *int64          `json:"assignmentGeneration,omitempty"`
}

// UpdateProjectStatus calls PATCH /api/v1/projects/{name}/status to report the
// observed phase to the server.  phase should be Running or Failed. The fence
// is presented so a report from an Agent that has lost ownership is rejected;
// a 409 comes back as ErrStaleAssignment, telling the caller to stop and wait
// for the next ownership poll rather than retry the same stale report.
func (c *Client) UpdateProjectStatus(ctx context.Context, projectName string, fence AssignmentFence, phase v1.ProjectPhase, reason, message string) error {
	gen := fence.Generation
	reqBody := projectStatusRequest{
		Phase:   phase,
		Reason:  reason,
		Message: message,
		UID:     fence.UID, NodeRef: fence.NodeRef, AssignmentGeneration: &gen,
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

	if resp.StatusCode == http.StatusConflict {
		return ErrStaleAssignment
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("update project status: unexpected status %s", resp.Status)
	}

	c.logger.Info("Project status updated",
		zap.String("project", projectName),
		zap.String("phase", string(phase)),
	)
	return nil
}

// ErrSecretNotFound is returned by GetSecret when the server responds with
// 404. resolveSecrets treats this as a terminal condition for the project
// (Failed, no retry at this layer) rather than a transient fetch error.
var ErrSecretNotFound = errors.New("secret not found")

// GetSecret fetches a single Secret by name via GET /api/v1/secrets/{name}.
// The returned Secret carries plaintext values; they must stay in process
// memory only — never written to disk or logs (see CARA-57 memory-only rule).
// Returns ErrSecretNotFound when the server responds with 404.
func (c *Client) GetSecret(ctx context.Context, name string) (*v1.Secret, error) {
	url := fmt.Sprintf("%s/api/v1/secrets/%s", c.serverURL, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build get secret request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get secret request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get secret %q: unexpected status %s", name, resp.Status)
	}

	var secret v1.Secret
	if err := json.NewDecoder(resp.Body).Decode(&secret); err != nil {
		return nil, fmt.Errorf("decode secret %q: %w", name, err)
	}
	return &secret, nil
}
