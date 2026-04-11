package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// Client communicates with cara-server on behalf of the CLI.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient returns a Client pointed at the given server address.
func NewClient(serverURL string) *Client {
	return &Client{
		BaseURL:    serverURL,
		HTTPClient: &http.Client{},
	}
}

// GetNodes fetches all nodes from the server.
func (c *Client) GetNodes(ctx context.Context) (v1.NodeList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/nodes", nil)
	if err != nil {
		return v1.NodeList{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.NodeList{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return v1.NodeList{}, err
	}

	var list v1.NodeList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return v1.NodeList{}, fmt.Errorf("decode response: %w", err)
	}
	return list, nil
}

// GetNode fetches a single node by name.
func (c *Client) GetNode(ctx context.Context, name string) (v1.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/nodes/"+name, nil)
	if err != nil {
		return v1.Node{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.Node{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return v1.Node{}, err
	}

	var node v1.Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return v1.Node{}, fmt.Errorf("decode response: %w", err)
	}
	return node, nil
}

// DeleteNode removes a node by name.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/v1/nodes/"+name, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	return checkStatus(resp)
}

// ApplyResult wraps the resource returned by an apply operation together with
// whether it was newly created or updated (configured).
type ApplyResult struct {
	Resource any
	Created  bool // true = POST created; false = PUT updated
}

// ApplyResource submits a raw YAML/JSON manifest to the server.
// The kind field in the manifest is used to route to the correct endpoint.
func (c *Client) ApplyResource(ctx context.Context, raw []byte) (ApplyResult, error) {
	// Decode just the TypeMeta to determine which endpoint to call.
	var meta v1.TypeMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ApplyResult{}, fmt.Errorf("decode kind: %w", err)
	}

	switch meta.Kind {
	case "Node":
		return c.applyNode(ctx, raw)
	case "Project":
		return c.applyProject(ctx, raw)
	default:
		return ApplyResult{}, fmt.Errorf("unsupported kind %q", meta.Kind)
	}
}

// applyNode creates or updates a Node manifest. It tries POST first; on 409
// Conflict it falls back to PUT to update the existing resource.
func (c *Client) applyNode(ctx context.Context, raw []byte) (ApplyResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/nodes",
		bytes.NewReader(raw))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// On 409, fall back to PUT for update.
	if resp.StatusCode == http.StatusConflict {
		_ = resp.Body.Close()

		var meta struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return ApplyResult{}, fmt.Errorf("decode name for update: %w", err)
		}

		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
			c.BaseURL+"/api/v1/nodes/"+meta.Metadata.Name, bytes.NewReader(raw))
		if err != nil {
			return ApplyResult{}, fmt.Errorf("build PUT request: %w", err)
		}
		putReq.Header.Set("Content-Type", "application/json")

		putResp, err := c.HTTPClient.Do(putReq)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("PUT request failed: %w", err)
		}
		defer putResp.Body.Close()

		if err := checkStatus(putResp); err != nil {
			return ApplyResult{}, err
		}

		var node v1.Node
		if err := json.NewDecoder(putResp.Body).Decode(&node); err != nil {
			return ApplyResult{}, fmt.Errorf("decode response: %w", err)
		}
		return ApplyResult{Resource: node, Created: false}, nil
	}

	if err := checkStatus(resp); err != nil {
		return ApplyResult{}, err
	}

	var node v1.Node
	if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
		return ApplyResult{}, fmt.Errorf("decode response: %w", err)
	}
	return ApplyResult{Resource: node, Created: true}, nil
}

// GetProjects fetches projects from the server. If phase is non-empty, only
// projects in that phase are returned (maps to the ?phase= query parameter).
func (c *Client) GetProjects(ctx context.Context, phase string) (v1.ProjectList, error) {
	url := c.BaseURL + "/api/v1/projects"
	if phase != "" {
		url += "?phase=" + phase
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return v1.ProjectList{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.ProjectList{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return v1.ProjectList{}, err
	}

	var list v1.ProjectList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return v1.ProjectList{}, fmt.Errorf("decode response: %w", err)
	}
	return list, nil
}

// GetProject fetches a single project by name.
func (c *Client) GetProject(ctx context.Context, name string) (v1.Project, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/projects/"+name, nil)
	if err != nil {
		return v1.Project{}, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.Project{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return v1.Project{}, err
	}

	var project v1.Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return v1.Project{}, fmt.Errorf("decode response: %w", err)
	}
	return project, nil
}

// DeleteProject removes a project by name.
// It returns (true, nil) when the project was deleted immediately (204 No Content)
// and (false, nil) when the project has been marked for graceful termination
// (202 Accepted).
func (c *Client) DeleteProject(ctx context.Context, name string) (deleted bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/v1/projects/"+name, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := checkStatus(resp); err != nil {
		return false, err
	}
	return resp.StatusCode == http.StatusNoContent, nil
}

// applyProject creates or updates a Project manifest. It tries POST first; on 409
// Conflict it falls back to PUT to update the existing resource.
func (c *Client) applyProject(ctx context.Context, raw []byte) (ApplyResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/projects",
		bytes.NewReader(raw))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// On 409, fall back to PUT for update.
	if resp.StatusCode == http.StatusConflict {
		_ = resp.Body.Close()

		var meta struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return ApplyResult{}, fmt.Errorf("decode name for update: %w", err)
		}

		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
			c.BaseURL+"/api/v1/projects/"+meta.Metadata.Name, bytes.NewReader(raw))
		if err != nil {
			return ApplyResult{}, fmt.Errorf("build PUT request: %w", err)
		}
		putReq.Header.Set("Content-Type", "application/json")

		putResp, err := c.HTTPClient.Do(putReq)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("PUT request failed: %w", err)
		}
		defer putResp.Body.Close()

		if err := checkStatus(putResp); err != nil {
			return ApplyResult{}, err
		}

		var project v1.Project
		if err := json.NewDecoder(putResp.Body).Decode(&project); err != nil {
			return ApplyResult{}, fmt.Errorf("decode response: %w", err)
		}
		return ApplyResult{Resource: project, Created: false}, nil
	}

	if err := checkStatus(resp); err != nil {
		return ApplyResult{}, err
	}

	var project v1.Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return ApplyResult{}, fmt.Errorf("decode response: %w", err)
	}
	return ApplyResult{Resource: project, Created: true}, nil
}

// checkStatus returns an error if the HTTP response indicates a failure.
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("server returned %s: %s", resp.Status, bytes.TrimSpace(body))
}
