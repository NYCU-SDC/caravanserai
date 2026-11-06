package main

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --output was the only user-facing flag in caractl without a shorthand, so
// the form every comparable CLI accepts failed with "unknown shorthand flag".
func TestOutputFlagHasAShorthand(t *testing.T) {
	f := rootCmd.PersistentFlags().Lookup("output")
	require.NotNil(t, f, "--output must be registered on the root command")
	assert.Equal(t, "o", f.Shorthand)
}

// Adding the shorthand must not change what the flag does, in either form.
func TestOutputShorthandAndLongFormAgree(t *testing.T) {
	assert.Equal(t, "yaml", parseOutput(t, "-o", "yaml"))
	assert.Equal(t, "yaml", parseOutput(t, "--output", "yaml"))
	assert.Equal(t, "table", parseOutput(t), "the default is unchanged")
}

// It is a persistent flag, so every command that reads --output through
// cmd.Root() gets the shorthand with it. This pins that none of them has
// taken -o for something of its own, which is how a persistent shorthand
// breaks.
func TestSubcommandsInheritTheOutputShorthand(t *testing.T) {
	for _, name := range []string{"get", "describe", "apply", "overlay"} {
		t.Run(name, func(t *testing.T) {
			cmd, _, err := rootCmd.Find([]string{name})
			require.NoError(t, err)
			require.Equal(t, name, cmd.Name())

			f := cmd.InheritedFlags().Lookup("output")
			require.NotNil(t, f, "%s must inherit --output", name)
			assert.Equal(t, "o", f.Shorthand)

			// What -o resolves to on this command, which is what a conflict
			// with a local flag of its own would change.
			short := cmd.Flags().ShorthandLookup("o")
			require.NotNil(t, short, "-o must resolve on %s", name)
			assert.Equal(t, "output", short.Name, "%s has taken -o for something else", name)
		})
	}
}

// parseOutput parses args against the real root flag set and returns what
// --output ends up as. The flag set is package state shared by every call, so
// it is reset first rather than afterwards — a deferred reset would not run
// between two calls inside one test.
func parseOutput(t *testing.T, args ...string) string {
	t.Helper()

	flags := rootCmd.PersistentFlags()
	require.NoError(t, flags.Set("output", flags.Lookup("output").DefValue))

	require.NoError(t, flags.Parse(args))
	value, err := flags.GetString("output")
	require.NoError(t, err)
	return value
}

// A typo below the top level used to print help and exit 0, which a script or
// an LLM agent reads as an empty success. Walking the real tree means a parent
// command added later is held to the same rule.
func TestEveryParentRejectsUnknownSubcommands(t *testing.T) {
	var walk func(c *cobra.Command)
	checked := 0
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !sub.HasSubCommands() {
				continue
			}
			path := strings.Fields(sub.CommandPath())[1:]
			t.Run(strings.Join(path, " "), func(t *testing.T) {
				rootCmd.SetArgs(append(append([]string{}, path...), "bogus"))
				rootCmd.SetOut(io.Discard)
				rootCmd.SetErr(io.Discard)
				err := rootCmd.Execute()
				require.Error(t, err, "caractl %s bogus must fail", strings.Join(path, " "))
				assert.Contains(t, err.Error(), `unknown subcommand "bogus"`)
				for _, s := range sub.Commands() {
					if s.IsAvailableCommand() {
						assert.Contains(t, err.Error(), s.Name(), "the message names the valid subcommands")
					}
				}
			})
			checked++
			walk(sub)
		}
	}
	walk(rootCmd)
	assert.GreaterOrEqual(t, checked, 6, "get, delete, describe, node, node token, overlay, token")
}

func TestTyposGetASuggestion(t *testing.T) {
	rootCmd.SetArgs([]string{"get", "projetcs"})
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	err := rootCmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `did you mean "projects"?`)
}

// A parent on its own still prints its help: that is what was asked for.
func TestBareParentPrintsHelp(t *testing.T) {
	var out strings.Builder
	rootCmd.SetArgs([]string{"get"})
	rootCmd.SetOut(&out)
	require.NoError(t, rootCmd.Execute())
	assert.Contains(t, out.String(), "Available Commands")
}

// The binary is caractl; it used to call itself caractrl.
func TestNoCommandSaysCaractrl(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{c.Use, c.Short, c.Long, c.Example} {
			assert.NotContains(t, text, "caractrl", "in %q", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	var out strings.Builder
	rootCmd.SetArgs([]string{"version"})
	rootCmd.SetOut(&out)
	require.NoError(t, rootCmd.Execute())
	assert.True(t, strings.HasPrefix(out.String(), "caractl "), "version prints %q", out.String())
}
