package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// NewDeleteCmd returns the "delete" subcommand tree.
//
// Usage:
//
//	caractrl delete node <name>
//	caractrl delete project <name>
func NewDeleteCmd() *cobra.Command {
	deleteCmd := &cobra.Command{
		Use:   "delete <resource> <name>",
		Short: "Delete a resource by name",
	}

	deleteCmd.AddCommand(newDeleteNodeCmd())
	deleteCmd.AddCommand(newDeleteProjectCmd())
	return deleteCmd
}

func newDeleteNodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "node <name>",
		Short:   "Delete a node",
		Aliases: []string{"nodes"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			name := args[0]

			client := NewClient(serverURL)
			ctx := context.Background()

			if err := client.DeleteNode(ctx, name); err != nil {
				return fmt.Errorf("delete node %q: %w", name, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "node %q deleted\n", name)
			return nil
		},
	}
}

func newDeleteProjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "project <name>",
		Short:   "Delete a project",
		Aliases: []string{"projects"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			name := args[0]

			client := NewClient(serverURL)
			ctx := context.Background()

			deleted, err := client.DeleteProject(ctx, name)
			if err != nil {
				return fmt.Errorf("delete project %q: %w", name, err)
			}

			if deleted {
				fmt.Fprintf(cmd.OutOrStdout(), "project %q deleted\n", name)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "project %q is being deleted\n", name)
			}
			return nil
		},
	}
}
