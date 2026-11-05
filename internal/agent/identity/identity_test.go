package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var errRefused = errors.New("refused")

// fakeJoiner hands out Node tokens for join tokens it knows, once each.
type fakeJoiner struct {
	tokens   map[string]string
	used     map[string]bool
	failures int // transient failures before answering
	calls    int
}

func (f *fakeJoiner) Join(_ context.Context, joinToken string) (string, error) {
	f.calls++
	if f.failures > 0 {
		f.failures--
		return "", errors.New("connection refused")
	}
	node, ok := f.tokens[joinToken]
	if !ok || f.used[joinToken] {
		return "", errRefused
	}
	if f.used == nil {
		f.used = map[string]bool{}
	}
	f.used[joinToken] = true
	return node, nil
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func config(t *testing.T, joinFile string) Config {
	return Config{
		JoinTokenFile: joinFile,
		Dir:           filepath.Join(t.TempDir(), "identity"),
		IsRefusal:     func(err error) bool { return errors.Is(err, errRefused) },
		RetryInterval: time.Millisecond,
	}
}

func TestFirstStartJoinsAndKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1\n"))
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}}

	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node1", token)

	info, err := os.Stat(filepath.Join(cfg.Dir, nodeTokenFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the kept token is readable by the agent only")
	dirInfo, err := os.Stat(cfg.Dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

// A restart must not need the join token again: it was used up.
func TestRestartUsesTheKeptTokenWithoutJoining(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}}
	_, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)

	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node1", token)
	assert.Equal(t, 1, joiner.calls, "the restart did not join again")

	cfg.JoinTokenFile = ""
	token, err = Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err, "the join token file may be removed after enrolment")
	assert.Equal(t, "cara_node1", token)
}

// A new join token in the same file enrols the Node again: how a revoked Node
// comes back.
func TestNewJoinTokenEnrolsAgain(t *testing.T) {
	dir := t.TempDir()
	joinFile := writeFile(t, dir, "join", "cara_join1")
	cfg := config(t, joinFile)
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1", "cara_join2": "cara_node2"}}
	_, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)

	writeFile(t, dir, "join", "cara_join2")
	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node2", token)

	token, err = Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node2", token, "the new token is the one kept")
}

// A refused new join token must not take away a Node token that still works.
func TestRefusedNewJoinTokenKeepsTheHeldToken(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}}
	_, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)

	writeFile(t, dir, "join", "cara_unknown")
	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node1", token)
}

// The crash keep() is ordered for: the token was written, its join hash was
// not. The join token is now used, so joining again is refused, and the kept
// token is used.
func TestCrashBeforeTheJoinHashKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}}
	_, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(cfg.Dir, joinedWithFile)))

	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node1", token)
}

func TestRefusedFirstJoinFails(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_used"))
	joiner := &fakeJoiner{tokens: map[string]string{}}

	_, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	assert.ErrorIs(t, err, errRefused)
	assert.Equal(t, 1, joiner.calls, "a refusal is not retried")
	_, statErr := os.Stat(filepath.Join(cfg.Dir, nodeTokenFile))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// cara-server may not be up yet when the agent starts.
func TestJoinRetriesUntilTheServerAnswers(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}, failures: 3}

	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_node1", token)
	assert.Equal(t, 4, joiner.calls)
}

func TestJoinStopsWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	joiner := &fakeJoiner{failures: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := Resolve(ctx, cfg, joiner, zap.NewNop())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestNoCredential(t *testing.T) {
	_, err := Resolve(context.Background(), config(t, ""), &fakeJoiner{}, zap.NewNop())
	assert.ErrorIs(t, err, ErrNoCredential)
}

func TestTokenFileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "cara_join1"))
	cfg.TokenFile = writeFile(t, dir, "node", " cara_given \n")
	joiner := &fakeJoiner{tokens: map[string]string{"cara_join1": "cara_node1"}}

	token, err := Resolve(context.Background(), cfg, joiner, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "cara_given", token)
	assert.Zero(t, joiner.calls)
}

func TestEmptyFilesAreRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := config(t, writeFile(t, dir, "join", "\n"))
	_, err := Resolve(context.Background(), cfg, &fakeJoiner{}, zap.NewNop())
	assert.ErrorContains(t, err, "is empty")

	cfg = config(t, "")
	cfg.TokenFile = writeFile(t, dir, "node", "")
	_, err = Resolve(context.Background(), cfg, &fakeJoiner{}, zap.NewNop())
	assert.ErrorContains(t, err, "is empty")
}
