//go:build e2e

package pgbackrest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Docker streams an exec's output until the command ends; the runtime must
// still give up at the caller's deadline, or one stuck command holds whatever
// waits for it.
func TestExecHonoursTheDeadlineOfASilentCommand(t *testing.T) {
	requireImage(t)
	runtime := newRuntime(t)
	name := fmt.Sprintf("cara-exec-test-%d", time.Now().UnixNano()%1_000_000)
	out, err := dockerRun(t, "run", "-d", "--name", name, "--entrypoint", "sleep", image(), "300")
	require.NoError(t, err, out)
	t.Cleanup(func() { _, _ = dockerRun(t, "rm", "-fv", name) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runtime.Exec(ctx, name, "postgres", []string{"sleep", "5"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// The PostgreSQL image declares PGDATA a volume, so every helper run from it
// gets an anonymous one; removing the helper must remove that too.
func TestHelperLeavesNoVolumeBehind(t *testing.T) {
	requireImage(t)
	runtime := newRuntime(t)
	volumes := func() map[string]bool {
		out, err := dockerRun(t, "volume", "ls", "-q")
		require.NoError(t, err, out)
		set := map[string]bool{}
		for _, v := range strings.Fields(out) {
			set[v] = true
		}
		return set
	}
	before := volumes()
	res, err := runtime.RunToCompletion(context.Background(), image(), "0", nil, []string{"true"})
	require.NoError(t, err)
	require.Zero(t, res.ExitCode)
	for v := range volumes() {
		assert.True(t, before[v], "volume %s was left by the helper", v)
	}
}
