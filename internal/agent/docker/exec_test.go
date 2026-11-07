package docker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newExecRuntime(f *fakeDocker) *DockerRuntime {
	return &DockerRuntime{client: f, logger: zap.NewNop()}
}

func TestExecReturnsOutputAndExitCode(t *testing.T) {
	f := newFakeDocker()
	f.execs = map[string]ExecResult{
		"pgbackrest version":          {ExitCode: 0, Stdout: "pgBackRest 2.59.3\n"},
		"pgbackrest --stanza=s check": {ExitCode: 28, Stderr: "ERROR: [028]: archive not found\n"},
	}
	r := newExecRuntime(f)

	ok, err := r.Exec(context.Background(), "db", "postgres", []string{"pgbackrest", "version"})
	require.NoError(t, err)
	assert.Equal(t, ExecResult{ExitCode: 0, Stdout: "pgBackRest 2.59.3\n"}, ok)
	assert.Equal(t, "postgres", f.lastExecUser)

	// A failing command is a result to read, not an error, and never success.
	failed, err := r.Exec(context.Background(), "db", "postgres", []string{"pgbackrest", "--stanza=s", "check"})
	require.NoError(t, err)
	assert.Equal(t, 28, failed.ExitCode)
	assert.Contains(t, failed.Stderr, "archive not found")
	assert.Empty(t, failed.Stdout, "stderr is not mixed into stdout")
}

func TestConnectNetworkIsIdempotent(t *testing.T) {
	f := newFakeDocker()
	f.networks["cara-blog"] = map[string]string{}
	r := newExecRuntime(f)
	ctx := context.Background()

	require.NoError(t, r.ConnectNetwork(ctx, "cara-blog", "repo", []string{"pgbackrest-repo"}))
	require.NoError(t, r.ConnectNetwork(ctx, "cara-blog", "repo", []string{"pgbackrest-repo"}))
	assert.Equal(t, []string{"NetworkConnect:cara-blog:repo"}, f.mutationsWithPrefix("Network"),
		"an attached container is not connected twice")

	attached, err := r.NetworkAttached(ctx, "cara-blog", "repo")
	require.NoError(t, err)
	assert.True(t, attached)
}

func TestDisconnectNetworkIsIdempotent(t *testing.T) {
	f := newFakeDocker()
	f.networks["cara-blog"] = map[string]string{}
	r := newExecRuntime(f)
	ctx := context.Background()

	require.NoError(t, r.ConnectNetwork(ctx, "cara-blog", "repo", nil))
	require.NoError(t, r.DisconnectNetwork(ctx, "cara-blog", "repo"))
	require.NoError(t, r.DisconnectNetwork(ctx, "cara-blog", "repo"), "not attached is not an error")
	require.NoError(t, r.DisconnectNetwork(ctx, "cara-gone", "repo"), "a removed network is not an error")

	attached, err := r.NetworkAttached(ctx, "cara-blog", "repo")
	require.NoError(t, err)
	assert.False(t, attached)
	assert.Equal(t, []string{"NetworkConnect:cara-blog:repo", "NetworkDisconnect:cara-blog:repo"},
		f.mutationsWithPrefix("Network"))
}

// mutationsWithPrefix returns the recorded mutations that start with prefix.
func (f *fakeDocker) mutationsWithPrefix(prefix string) []string {
	var out []string
	for _, m := range f.mutations {
		if len(m) >= len(prefix) && m[:len(prefix)] == prefix {
			out = append(out, m)
		}
	}
	return out
}

// A helper or Node service can be the first thing on a Node to need its image:
// a missing image is pulled once, and one already present is not pulled.
func TestNodeContainersPullAMissingImage(t *testing.T) {
	f := newFakeDocker()
	f.missingImages = map[string]bool{"cara-postgres:16": true}
	r := newExecRuntime(f)

	_, err := r.RunToCompletion(context.Background(), "cara-postgres:16", "0", nil, []string{"true"})
	require.NoError(t, err)
	_, err = r.EnsureNodeContainer(context.Background(), NodeContainerSpec{Name: "repo", Image: "cara-postgres:16", Network: "n"})
	require.NoError(t, err)

	pulls := 0
	for _, m := range f.mutations {
		if m == "ImagePull:cara-postgres:16" {
			pulls++
		}
	}
	assert.Equal(t, 1, pulls)
}

// A command that runs on without writing anything still returns as soon as
// the caller's deadline passes.
func TestExecReturnsAtTheDeadlineOfASilentCommand(t *testing.T) {
	f := newFakeDocker()
	f.execHangs = true
	r := newExecRuntime(f)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Exec(ctx, "db", "postgres", []string{"sleep", "infinity"})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

// A helper is removed with its anonymous volumes whether it succeeds, fails
// or is abandoned by its caller.
func TestHelperIsRemovedWithItsVolumes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeDocker)
		ctx   func() (context.Context, context.CancelFunc)
	}{
		{"succeeds", func(*fakeDocker) {}, nil},
		{"fails", func(f *fakeDocker) { f.waitCode = 1 }, nil},
		{"abandoned", func(f *fakeDocker) { f.waitHangs = true }, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			tc.setup(f)
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			_, _ = newExecRuntime(f).RunToCompletion(ctx, "cara-postgres:16", "0", nil, []string{"true"})
			assert.Len(t, f.removedVolumesOf, 1)
		})
	}
}
