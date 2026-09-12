package main

import (
	"testing"

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
