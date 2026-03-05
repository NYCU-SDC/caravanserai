package main

import (
	"fmt"
	"os"

	"NYCU-SDC/caravanserai/internal/cli"

	"github.com/spf13/cobra"
)

// Build-time variables injected by the Makefile via -ldflags.
var (
	Version    = "dev"
	CommitHash = "unknown"
	BuildTime  = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "caractrl",
	Short: "caractrl — CLI for the Caravanserai control plane",
	Long: `caractrl is the command-line interface for Caravanserai.
It communicates with cara-server to manage Nodes, Projects, and cluster state.`,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("caractrl %s (commit %s, built %s)\n", Version, CommitHash, BuildTime)
	},
}

func init() {
	// Global persistent flags available to every subcommand.
	rootCmd.PersistentFlags().String("server", "http://localhost:8080", "cara-server address")
	rootCmd.PersistentFlags().String("output", "table", "output format: table | json | yaml")

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(cli.NewGetCmd())
	rootCmd.AddCommand(cli.NewDeleteCmd())
	rootCmd.AddCommand(cli.NewApplyCmd())
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
