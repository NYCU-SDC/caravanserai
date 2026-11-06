package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// RejectUnknownSubcommands makes every parent command below root fail on an
// argument that is not one of its subcommands.
//
// cobra answers an unknown top-level command with an error, but a parent
// further down that has no Run of its own prints its help and exits 0:
// `caractl get vms` looked like a successful, empty answer to a script or an
// LLM agent. A parent called with no argument still prints its help and
// exits 0, because the help is then what was asked for.
//
// It walks the tree once, after every command is added, so a parent added
// later is covered without remembering to.
func RejectUnknownSubcommands(root *cobra.Command) {
	for _, c := range root.Commands() {
		rejectUnknownBelow(c)
	}
}

func rejectUnknownBelow(c *cobra.Command) {
	if !c.HasSubCommands() {
		return
	}
	for _, sub := range c.Commands() {
		rejectUnknownBelow(sub)
	}
	if c.Run != nil || c.RunE != nil {
		return
	}
	c.Args = cobra.ArbitraryArgs
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		cmd.SilenceUsage = true
		return unknownSubcommand(cmd, args[0])
	}
}

func unknownSubcommand(cmd *cobra.Command, typed string) error {
	var valid []string
	for _, sub := range cmd.Commands() {
		if sub.IsAvailableCommand() {
			valid = append(valid, sub.Name())
		}
	}
	msg := fmt.Sprintf("unknown subcommand %q for %q; valid: %s", typed, cmd.CommandPath(), strings.Join(valid, ", "))
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	if s := cmd.SuggestionsFor(typed); len(s) > 0 {
		msg += fmt.Sprintf(" (did you mean %q?)", s[0])
	}
	return fmt.Errorf("%s", msg)
}
