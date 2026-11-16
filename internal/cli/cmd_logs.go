package cli

import (
	"context"
	"errors"
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
//	caractl logs <project>/<service> [--follow] [--tail N] [--timestamps] [--node <name>]
func NewLogsCmd() *cobra.Command {
	var (
		node       string
		follow     bool
		tail       string
		timestamps bool
	)

	cmd := &cobra.Command{
		Use:   "logs <project>/<service>",
		Short: "Print the logs of a container on a remote Node",
		Long: `Print the logs of a container. cara-server relays them from the Agent on
the Node running the Project, so this needs only a token for cara-server; the
Agent accepts requests from cara-server alone. This works similarly to
kubectl logs.

Examples:
  # Print all logs from the "web" service in project "my-app"
  caractl logs my-app/web

  # Stream logs in real-time (like tail -f)
  caractl logs my-app/web --follow

  # Show only the last 100 lines
  caractl logs my-app/web --tail 100

  # Show timestamps with each log line
  caractl logs my-app/web --timestamps

  # Read from a Node other than the one the Project is assigned to, such as
  # the one it just moved away from
  caractl logs my-app/web --node main`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, service, err := parseServiceTarget(args[0])
			if err != nil {
				return err
			}
			if err := checkNodeName(node); err != nil {
				return err
			}
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return streamLogs(ctx, NewClient(serverURL), project, service, node, follow, tail, timestamps, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node to read from; the Project's assigned Node if omitted")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream logs in real-time (follow)")
	cmd.Flags().StringVar(&tail, "tail", "all", "Number of lines to show from the end of the logs (default \"all\")")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "Show timestamps with each log line")

	return cmd
}

// parseServiceTarget splits "<project>/<service>".
func parseServiceTarget(arg string) (string, string, error) {
	project, service, ok := strings.Cut(arg, "/")
	if !ok || project == "" || service == "" || strings.Contains(service, "/") {
		return "", "", fmt.Errorf("invalid target %q: expected <project>/<service>", arg)
	}
	return project, service, nil
}

// checkNodeName refuses an agent address in --node. Before CARA-139 caractl
// dialed the Agent itself and --node took host:port; the Agent now accepts
// only cara-server, so an address cannot work.
func checkNodeName(node string) error {
	if strings.Contains(node, ":") {
		return fmt.Errorf("--node %q looks like an agent address: caractl reaches agents through cara-server now, and agents accept no one else; pass a Node name or leave --node out", node)
	}
	return nil
}

// serviceURL is the server route for a service of project; rest follows
// "/services/<service>/".
func (c *Client) serviceURL(project, service, rest string, query url.Values) string {
	u := c.BaseURL + "/api/v1/projects/" + url.PathEscape(project) + "/services/" + url.PathEscape(service) + "/" + rest
	if encoded := query.Encode(); encoded != "" {
		u += "?" + encoded
	}
	return u
}

func streamLogs(ctx context.Context, client *Client, project, service, node string, follow bool, tail string, timestamps bool, out io.Writer) error {
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
	if node != "" {
		query.Set("node", node)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.serviceURL(project, service, "logs", query), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// Ctrl+C.
			return nil
		}
		return fmt.Errorf("request to cara-server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkStatus(resp); err != nil {
		return err
	}
	_, err = io.Copy(out, resp.Body)
	if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled)) {
		return nil
	}
	return err
}
