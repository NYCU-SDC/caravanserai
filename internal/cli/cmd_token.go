package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"text/tabwriter"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

// NewTokenCmd returns the "token" command group, which issues and revokes the
// API tokens cara-server authenticates callers with (ADR-0031). Every
// subcommand needs an admin token in CARA_TOKEN.
func NewTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Issue, list and revoke API tokens",
	}
	cmd.AddCommand(newTokenCreateCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return cmd
}

func newTokenCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create node|user <name>",
		Short: "Issue a token and print it",
		Long: `create issues a token for a Node's Agent or for a person and prints it.
It is shown only this once; cara-server keeps only its hash.

A user token goes in ` + TokenEnv + `. Every user is an admin for now.

To enrol an Agent, prefer "caractl node token create": the agent trades that
one-time join token for its own Node token. A Node token issued here goes in the
file the agent's AGENT_TOKEN_FILE names.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			created, err := NewClient(serverURL).createToken(cmd.Context(), v1.APITokenKind(args[0]), args[1])
			if err != nil {
				return fmt.Errorf("create token: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), created.Token)
			fmt.Fprintf(cmd.ErrOrStderr(), "token %d for %s:%s issued; it will not be shown again\n",
				created.ID, created.Kind, created.Name)
			return nil
		},
	}
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List issued tokens, without the tokens themselves",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")
			tokens, err := NewClient(serverURL).listTokens(cmd.Context())
			if err != nil {
				return fmt.Errorf("list tokens: %w", err)
			}
			switch outputFmt {
			case "json":
				return printJSON(cmd.OutOrStdout(), tokens)
			case "yaml":
				return printYAML(cmd.OutOrStdout(), tokens)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tPRINCIPAL\tCREATED BY\tCREATED\tREVOKED")
			for _, t := range tokens {
				revoked := "-"
				if t.RevokedAt != nil {
					revoked = t.RevokedAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%d\t%s:%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Name, t.CreatedBy,
					t.CreatedAt.Local().Format("2006-01-02 15:04"), revoked)
			}
			return w.Flush()
		},
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token; its next request is refused",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("token id must be an integer: %q", args[0])
			}
			if err := NewClient(serverURL).revokeToken(cmd.Context(), id); err != nil {
				return fmt.Errorf("revoke token %d: %w", id, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token %d revoked\n", id)
			return nil
		},
	}
}

func (c *Client) createToken(ctx context.Context, kind v1.APITokenKind, name string) (v1.APITokenCreated, error) {
	body, err := json.Marshal(v1.APITokenRequest{Kind: kind, Name: name})
	if err != nil {
		return v1.APITokenCreated{}, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/tokens", bytes.NewReader(body))
	if err != nil {
		return v1.APITokenCreated{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.APITokenCreated{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return v1.APITokenCreated{}, err
	}
	var created v1.APITokenCreated
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return v1.APITokenCreated{}, fmt.Errorf("decode response: %w", err)
	}
	return created, nil
}

func (c *Client) listTokens(ctx context.Context) ([]v1.APIToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/tokens", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var tokens []v1.APIToken
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return tokens, nil
}

func (c *Client) revokeToken(ctx context.Context, id int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/api/v1/tokens/"+strconv.FormatInt(id, 10), nil)
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
