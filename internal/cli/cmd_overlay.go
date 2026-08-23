// cmd_overlay.go implements the "overlay" subcommand tree for Headscale node
// lifecycle operations (CARA-49).
//
// Routes:
//
//	caractl overlay create-preauth-key --node <name> [--ttl <dur>]
//	caractl overlay nodes list
//	caractl overlay nodes revoke <name>
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// NewOverlayCmd returns the "overlay" subcommand tree.
func NewOverlayCmd() *cobra.Command {
	overlayCmd := &cobra.Command{
		Use:   "overlay",
		Short: "Manage Headscale overlay nodes and pre-auth keys",
	}

	overlayCmd.AddCommand(newCreatePreAuthKeyCmd())
	overlayCmd.AddCommand(newOverlayNodesCmd())
	return overlayCmd
}

func newCreatePreAuthKeyCmd() *cobra.Command {
	var node, ttl string

	cmd := &cobra.Command{
		Use:   "create-preauth-key --node <name>",
		Short: "Issue a Headscale pre-auth key for a node to join the overlay",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			client := NewClient(serverURL)
			ctx := context.Background()

			key, err := client.CreateOverlayPreAuthKey(ctx, node, ttl)
			if err != nil {
				return fmt.Errorf("create pre-auth key: %w", err)
			}

			// The key is shown once; it is never stored or logged by caractl.
			fmt.Fprintln(os.Stdout, key.Key)
			if !key.Expiration.IsZero() {
				fmt.Fprintf(os.Stderr, "# expires %s\n", key.Expiration.UTC().Format("2006-01-02T15:04:05Z"))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Cara node name the key is intended for")
	cmd.Flags().StringVar(&ttl, "ttl", "", "key lifetime as a Go duration (e.g. 24h); empty uses the server default")
	return cmd
}

func newOverlayNodesCmd() *cobra.Command {
	nodesCmd := &cobra.Command{
		Use:   "nodes",
		Short: "List or revoke overlay nodes",
	}
	nodesCmd.AddCommand(newOverlayNodesListCmd())
	nodesCmd.AddCommand(newOverlayNodesRevokeCmd())
	return nodesCmd
}

func newOverlayNodesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List overlay nodes known to Headscale",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")

			client := NewClient(serverURL)
			printer := &Printer{Format: outputFmt, Out: os.Stdout}
			ctx := context.Background()

			list, err := client.ListOverlayNodes(ctx)
			if err != nil {
				return fmt.Errorf("list overlay nodes: %w", err)
			}
			return printer.PrintOverlayNodeList(list)
		},
	}
}

func newOverlayNodesRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke an overlay node from both Headscale and the Cara node store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			client := NewClient(serverURL)
			ctx := context.Background()

			res, err := client.RevokeOverlayNode(ctx, args[0])
			if err != nil {
				return fmt.Errorf("revoke overlay node %q: %w", args[0], err)
			}

			if res.Drift {
				// Partial success: surface it loudly and fail so scripts notice.
				fmt.Fprintf(os.Stderr, "WARNING: %s\n", res.Message)
				return fmt.Errorf("revoke overlay node %q incomplete: drift between Headscale and the node store", args[0])
			}

			fmt.Fprintf(os.Stdout, "revoked %q from the overlay\n", args[0])
			return nil
		},
	}
}
