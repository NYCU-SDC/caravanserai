package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

// NewNodeCmd returns the "node" subcommand tree.
//
// Usage:
//
//	caractl node probe <name>
//
// Additional node subcommands can be added over time; the tree is intentionally
// separate from `get nodes` / `describe nodes` because those are read-only
// resource-oriented actions, whereas subcommands under `node` may take
// active side-effects on the cluster.
func NewNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Node-level operations",
	}
	cmd.AddCommand(newNodeProbeCmd())
	return cmd
}

// probeResponse mirrors the server-side response body of
// POST /api/v1/nodes/{name}/probe.
type probeResponse struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode"`
	LatencyMs  int64  `json:"latencyMs"`
	Address    string `json:"address"`
	Error      string `json:"error,omitempty"`
}

func newNodeProbeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe <name>",
		Short: "Ask cara-server to dial the agent for a node and report reachability",
		Long: `probe asks cara-server to make a live HTTP call to the named node's agent
via its configured Dialer (Headscale overlay transport via tsnet).

Use this to verify that:
  - the Node record contains a reachable Status.Network.OverlayIP,
  - the server can reach the agent on that overlay address, and
  - the agent's HTTP server is responding to /healthz.

Exit code is 0 when OK=true, 1 otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			result, err := probeNode(cmd.Context(), serverURL, args[0])
			if err != nil {
				return fmt.Errorf("probe node %q: %w", args[0], err)
			}

			// Print a concise summary. Machine-readable output can be added
			// later via the top-level -o flag; for now the text form is
			// enough for humans and shell scripts (grep OK).
			status := "OK"
			if !result.OK {
				status = "FAIL"
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"%s  node=%s  address=%s  agentStatus=%d  latency=%dms\n",
				status, args[0], result.Address, result.StatusCode, result.LatencyMs,
			)
			if result.Error != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "error: %s\n", result.Error)
			}

			if !result.OK {
				// Signal failure to the shell so `caractl node probe ...`
				// can be used in health scripts.
				os.Exit(1)
			}
			return nil
		},
	}
}

func probeNode(ctx context.Context, serverURL, name string) (*probeResponse, error) {
	client := NewClient(serverURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		client.BaseURL+"/api/v1/nodes/"+name+"/probe", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// The probe endpoint returns 200 on both success and agent failure;
	// non-2xx indicates a server-side problem (e.g. unknown node → 404,
	// no dialer address yet → 409). Reuse ParseAPIError for consistency.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out probeResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return &out, nil
	}

	// Fall back to standard error handling for 4xx/5xx.
	return nil, checkStatus(resp)
}
