package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

// newNodeTokenCmd returns "node token", which enrols Agents (CARA-138).
func newNodeTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Issue one-time join tokens that enrol an Agent as a Node",
	}
	cmd.AddCommand(newNodeTokenCreateCmd())
	return cmd
}

func newNodeTokenCreateCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "create <node>",
		Short: "Issue a one-time join token for a Node and print it",
		Long: `create issues a join token bound to one Node name and prints it. It is shown
only this once.

Put it in the file the agent's JOIN_TOKEN_FILE names and start the agent. On
its first start the agent trades it for a Node token, keeps that under its data
root, and needs the join token no more. A join token works once, only for the
Node it names, and not after --ttl.

To enrol a Node again, for example after "caractl node revoke", issue a new join
token, replace the file's contents and restart the agent.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			created, err := NewClient(serverURL).createJoinToken(cmd.Context(), args[0], ttl)
			if err != nil {
				return fmt.Errorf("create join token: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), created.Token)
			fmt.Fprintf(cmd.ErrOrStderr(), "join token %d for node %s issued, valid until %s; it will not be shown again\n",
				created.ID, created.NodeName, created.ExpiresAt.Local().Format("2006-01-02 15:04"))
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", time.Hour, "how long the join token works (at most 168h)")
	return cmd
}

func newNodeRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <node>",
		Short: "Revoke a Node's tokens; its Agent's next request is refused",
		Long: `revoke stops every token of the Node from working: its Node tokens and any
join token for it not yet used. The Agent's next request gets 401, and it logs
that its token was refused. The Node record and its Projects are not touched.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			revoked, err := NewClient(serverURL).revokeNodeTokens(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("revoke node %s: %w", args[0], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node %s: %d node token(s) and %d unused join token(s) revoked\n",
				args[0], revoked.NodeTokens, revoked.JoinTokens)
			return nil
		},
	}
}

func (c *Client) createJoinToken(ctx context.Context, node string, ttl time.Duration) (v1.NodeJoinTokenCreated, error) {
	body, err := json.Marshal(v1.NodeJoinTokenRequest{TTL: ttl.String()})
	if err != nil {
		return v1.NodeJoinTokenCreated{}, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/nodes/"+url.PathEscape(node)+"/join-tokens", bytes.NewReader(body))
	if err != nil {
		return v1.NodeJoinTokenCreated{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.NodeJoinTokenCreated{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return v1.NodeJoinTokenCreated{}, err
	}
	var created v1.NodeJoinTokenCreated
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return v1.NodeJoinTokenCreated{}, fmt.Errorf("decode response: %w", err)
	}
	return created, nil
}

func (c *Client) revokeNodeTokens(ctx context.Context, node string) (v1.NodeTokensRevoked, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/api/v1/nodes/"+url.PathEscape(node)+"/tokens", nil)
	if err != nil {
		return v1.NodeTokensRevoked{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.NodeTokensRevoked{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return v1.NodeTokensRevoked{}, err
	}
	var revoked v1.NodeTokensRevoked
	if err := json.NewDecoder(resp.Body).Decode(&revoked); err != nil {
		return v1.NodeTokensRevoked{}, fmt.Errorf("decode response: %w", err)
	}
	return revoked, nil
}
