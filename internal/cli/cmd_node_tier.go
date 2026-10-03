package cli

import (
	"context"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

func newNodeSetTierCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-tier <name> primary|backup",
		Short: "Set a node's scheduling tier (the cara.io/tier label)",
		Long: `set-tier sets the cara.io/tier label on a node. The scheduler prefers
primary nodes and falls back to backup nodes. A node without the label is
treated as backup.

Only the tier label is changed; the node's spec (e.g. unschedulable) and its
other labels are kept.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")

			name, tier := args[0], v1.NodeTier(args[1])
			if err := setNodeTier(cmd.Context(), NewClient(serverURL), name, tier); err != nil {
				return fmt.Errorf("set tier of node %q: %w", name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node/%s tier set to %s\n", name, tier)
			return nil
		},
	}
}

// setNodeTier reads the node, sets its tier label and writes it back. The
// write replaces the node's spec and labels, so it is built from the node as
// read to keep everything but the tier unchanged.
func setNodeTier(ctx context.Context, client *Client, name string, tier v1.NodeTier) error {
	if !tier.IsValid() {
		return fmt.Errorf("tier must be %q or %q, got %q", v1.NodeTierPrimary, v1.NodeTierBackup, tier)
	}

	node, err := client.GetNode(ctx, name)
	if err != nil {
		return err
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[v1.LabelNodeTier] = string(tier)

	_, err = client.UpdateNode(ctx, node)
	return err
}
