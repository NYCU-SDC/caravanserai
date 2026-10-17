//go:build e2e

package pgbackrest

import (
	"context"
	"fmt"
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
