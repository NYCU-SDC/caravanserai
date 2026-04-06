package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// NewLogsCmd returns the "logs" command.
//
// Usage:
//
//	caractrl logs <project>/<service> [--follow] [--tail N] [--timestamps] [--node <agentAddr>]
func NewLogsCmd() *cobra.Command {
	var (
		nodeAddr   string
		follow     bool
		tail       string
		timestamps bool
	)

	cmd := &cobra.Command{
		Use:   "logs <project>/<service>",
		Short: "Print the logs of a container on a remote Node",
		Long: `Print the logs of a container running on a remote Node via the Agent's
log streaming endpoint.  This works similarly to kubectl logs.

Examples:
  # Print all logs from the "web" service in project "my-app"
  caractrl logs my-app/web

  # Stream logs in real-time (like tail -f)
  caractrl logs my-app/web --follow

  # Show only the last 100 lines
  caractrl logs my-app/web --tail 100

  # Show timestamps with each log line
  caractl logs my-app/web --timestamps

  # Specify Agent by node name (resolved via server API)
  caractrl logs my-app/web --node main

  # Specify Agent address explicitly
  caractrl logs my-app/web --node 192.168.1.100:9090`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, args, nodeAddr, follow, tail, timestamps)
		},
	}

	cmd.Flags().StringVar(&nodeAddr, "node", "", "Node name or agent address (host:port); auto-resolved from server if omitted")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream logs in real-time (follow)")
	cmd.Flags().StringVar(&tail, "tail", "all", "Number of lines to show from the end of the logs (default \"all\")")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "Show timestamps with each log line")

	return cmd
}

func runLogs(cmd *cobra.Command, args []string, nodeAddr string, follow bool, tail string, timestamps bool) error {
	// Parse <project>/<service>
	parts := strings.SplitN(args[0], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid target %q: expected <project>/<service>", args[0])
	}
	project, service := parts[0], parts[1]

	// Resolve agent address (same pattern as port-forward).
	if nodeAddr == "" {
		// Auto-resolve from project's nodeRef.
		serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
		resolved, resolveErr := resolveAgentAddr(serverURL, project)
		if resolveErr != nil {
			return resolveErr
		}
		nodeAddr = resolved
	} else if !strings.Contains(nodeAddr, ":") {
		// Treat as a node name — resolve via the server API.
		serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
		resolved, resolveErr := resolveNodeAddr(serverURL, nodeAddr)
		if resolveErr != nil {
			return resolveErr
		}
		nodeAddr = resolved
	}
	// else: treat as raw host:port

	// Set up signal handling for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build the request URL.
	query := url.Values{}
	if follow {
		query.Set("follow", "true")
	}
	if tail != "all" && tail != "" {
		query.Set("tail", tail)
	}
	if timestamps {
		query.Set("timestamps", "true")
	}

	reqURL := fmt.Sprintf("http://%s/api/v1/logs/%s/%s", nodeAddr, project, service)
	if encoded := query.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// User cancelled with Ctrl+C.
			return nil
		}
		return fmt.Errorf("request to agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", extractLogsProblemDetail(body, resp.Status))
	}

	// Stream the response body to stdout.
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil && ctx.Err() != nil {
		// Context cancelled (Ctrl+C) — not an error.
		return nil
	}
	return err
}

// extractLogsProblemDetail attempts to parse an RFC 7807 problem+json body and
// return the human-readable detail field.  Falls back to the raw body if
// parsing fails.
func extractLogsProblemDetail(body []byte, httpStatus string) string {
	var p struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(body, &p); err == nil && p.Detail != "" {
		return p.Detail
	}
	// Fallback: return raw body with HTTP status.
	return fmt.Sprintf("%s: %s", httpStatus, strings.TrimSpace(string(body)))
}
