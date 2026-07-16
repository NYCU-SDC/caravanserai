package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// NewGetCmd returns the "get" subcommand tree.
//
// Usage:
//
//	caractrl get nodes
//	caractrl get nodes <name>
//	caractrl get projects [--phase <phase>]
//	caractrl get projects <name>
func NewGetCmd() *cobra.Command {
	getCmd := &cobra.Command{
		Use:   "get <resource> [name]",
		Short: "Display one or many resources",
	}

	getCmd.AddCommand(newGetNodesCmd())
	getCmd.AddCommand(newGetProjectsCmd())
	getCmd.AddCommand(newGetSecretsCmd())
	return getCmd
}

func newGetSecretsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "secrets [name]",
		Short:   "List secrets or get a single secret (values are never shown)",
		Aliases: []string{"secret"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")

			client := NewClient(serverURL)
			printer := &Printer{Format: outputFmt, Out: os.Stdout}
			ctx := context.Background()

			if len(args) == 1 {
				secret, err := client.GetSecret(ctx, args[0])
				if err != nil {
					return fmt.Errorf("get secret %q: %w", args[0], err)
				}
				return printer.PrintSecret(secret)
			}

			list, err := client.GetSecrets(ctx)
			if err != nil {
				return fmt.Errorf("get secrets: %w", err)
			}
			return printer.PrintSecretList(list)
		},
	}
}

func newGetNodesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "nodes [name]",
		Short:   "List nodes or get a single node",
		Aliases: []string{"node"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")

			client := NewClient(serverURL)
			printer := &Printer{Format: outputFmt, Out: os.Stdout}
			ctx := context.Background()

			if len(args) == 1 {
				node, err := client.GetNode(ctx, args[0])
				if err != nil {
					return fmt.Errorf("get node %q: %w", args[0], err)
				}
				return printer.PrintNode(node)
			}

			list, err := client.GetNodes(ctx)
			if err != nil {
				return fmt.Errorf("get nodes: %w", err)
			}
			return printer.PrintNodeList(list)
		},
	}
}

func newGetProjectsCmd() *cobra.Command {
	var phase string

	cmd := &cobra.Command{
		Use:     "projects [name]",
		Short:   "List projects or get a single project",
		Aliases: []string{"project"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			outputFmt, _ := cmd.Root().PersistentFlags().GetString("output")

			client := NewClient(serverURL)
			printer := &Printer{Format: outputFmt, Out: os.Stdout}
			ctx := context.Background()

			if len(args) == 1 {
				project, err := client.GetProject(ctx, args[0])
				if err != nil {
					return fmt.Errorf("get project %q: %w", args[0], err)
				}
				return printer.PrintProject(project)
			}

			list, err := client.GetProjects(ctx, phase)
			if err != nil {
				return fmt.Errorf("get projects: %w", err)
			}
			return printer.PrintProjectList(list)
		},
	}

	cmd.Flags().StringVar(&phase, "phase", "", "filter by phase (Pending, Scheduled, Running, Failed, Terminating)")
	return cmd
}
