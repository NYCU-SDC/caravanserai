package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewRolloutCmd returns the "rollout" command group.
func NewRolloutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollout",
		Short: "Manage how a project's containers are replaced",
	}
	cmd.AddCommand(newRolloutRestartCmd())
	return cmd
}

func newRolloutRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <project>",
		Short: "Recreate every service of a project",
		Long: `restart records the time in the project's spec.restartedAt, which makes a new
spec revision. The project's Node then recreates every service container, one
service at a time, stopping each before starting its replacement.

Use it after changing a Secret the project reads: a Secret change alone does not
recreate containers. A later apply of the project's manifest keeps the restart
time, so it does not restart again.

Watch it with "caractl get project <project>": the SPEC column shows the new
revision with "(running N)" until every service runs it.`,
		Example: `  caractl rollout restart web`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			client := NewClient(serverURL)
			project, err := client.RestartProject(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("restart project %q: %w", args[0], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project/%s restarted: spec revision %d\n", project.Name, project.SpecRevision)
			return nil
		},
	}
}
