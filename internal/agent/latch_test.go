package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func latchedAssignment(name, uid string, generation int64) *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: "node-a", AssignmentGeneration: generation},
	}
}

func TestStopLatchesHoldOnlyTheRecordedAssignment(t *testing.T) {
	latches := NewStopLatches(t.TempDir())
	require.NoError(t, latches.Record(latchedAssignment("blog", "uid-1", 4), time.Now()))

	for name, tc := range map[string]struct {
		p    *v1.Project
		want bool
	}{
		"the recorded assignment": {latchedAssignment("blog", "uid-1", 4), true},
		"a later generation":      {latchedAssignment("blog", "uid-1", 5), false},
		"a new lifetime":          {latchedAssignment("blog", "uid-2", 4), false},
		"another Project":         {latchedAssignment("shop", "uid-1", 4), false},
	} {
		t.Run(name, func(t *testing.T) {
			held, err := latches.Holds(tc.p)
			require.NoError(t, err)
			assert.Equal(t, tc.want, held)
		})
	}

	require.NoError(t, latches.Forget("default", "blog"))
	held, err := latches.Holds(latchedAssignment("blog", "uid-1", 4))
	require.NoError(t, err)
	assert.False(t, held)
	assert.NoError(t, latches.Forget("default", "blog"), "forgetting twice is fine")
}

// Kept across an Agent restart: a new StopLatches over the same directory
// reads what the old one wrote.
func TestStopLatchesSurviveARestart(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, NewStopLatches(root).Record(latchedAssignment("blog", "uid-1", 4), time.Now()))

	held, err := NewStopLatches(root).Holds(latchedAssignment("blog", "uid-1", 4))
	require.NoError(t, err)
	assert.True(t, held)
}

// A record that cannot be read refuses starts rather than allowing them.
func TestStopLatchesFailClosedOnAnUnreadableRecord(t *testing.T) {
	root := t.TempDir()
	latches := NewStopLatches(root)
	path := filepath.Join(root, "stop-latches", "default", "blog.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{half"), 0o600))

	held, err := latches.Holds(latchedAssignment("blog", "uid-1", 4))
	assert.Error(t, err)
	assert.True(t, held)
	assert.ErrorIs(t, latches.refuseStart(latchedAssignment("blog", "uid-1", 4)), ErrWorkloadLatched)

	// Sync leaves it for an operator instead of deleting it.
	latches.Sync(nil, time.Now(), zap.NewNop())
	assert.FileExists(t, path)
}

func TestStopLatchesRefuseUnusableKeys(t *testing.T) {
	latches := NewStopLatches(t.TempDir())
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		assert.Error(t, latches.Record(latchedAssignment(name, "uid-1", 1), time.Now()), "name %q", name)
	}
}

func TestStopLatchesSync(t *testing.T) {
	latches := NewStopLatches(t.TempDir())
	now := time.Now()
	for _, p := range []*v1.Project{
		latchedAssignment("kept", "uid-1", 4),
		latchedAssignment("moved-away", "uid-2", 1),
		latchedAssignment("reassigned", "uid-3", 1),
		latchedAssignment("recreated", "uid-4", 1),
	} {
		require.NoError(t, latches.Record(p, now))
	}
	// Evidence the server holds that this node has no record of: written by
	// an Agent from before these records existed.
	unrecorded := latchedAssignment("unrecorded", "uid-5", 2)
	latch(unrecorded, 2)

	latches.Sync([]*v1.Project{
		latchedAssignment("kept", "uid-1", 4),
		latchedAssignment("reassigned", "uid-3", 2),
		latchedAssignment("recreated", "uid-9", 1),
		unrecorded,
	}, now, zap.NewNop())

	holds := func(p *v1.Project) bool {
		held, err := latches.Holds(p)
		require.NoError(t, err)
		return held
	}
	assert.True(t, holds(latchedAssignment("kept", "uid-1", 4)))
	assert.False(t, holds(latchedAssignment("moved-away", "uid-2", 1)), "no longer assigned here")
	assert.False(t, holds(latchedAssignment("reassigned", "uid-3", 1)), "a newer generation holds the grant")
	assert.False(t, holds(latchedAssignment("recreated", "uid-4", 1)), "another lifetime holds the name")
	assert.True(t, holds(unrecorded), "server-held evidence is recorded locally")

	entries, err := os.ReadDir(filepath.Join(latches.dir, "default"))
	require.NoError(t, err)
	assert.Len(t, entries, 2, "dropped records leave no files behind")
}

// No start reaches Docker for a latched assignment, whichever start it is.
// The embedded runtime is nil, so reaching it would panic.
func TestGuardedRuntimeRefusesEveryStartOfALatchedAssignment(t *testing.T) {
	latches := NewStopLatches(t.TempDir())
	p := latchedAssignment("blog", "uid-1", 4)
	require.NoError(t, latches.Record(p, time.Now()))
	g := GuardRuntime(nil, latches)
	ctx := context.Background()

	assert.ErrorIs(t, g.ReconcileProject(ctx, p), ErrWorkloadLatched)
	assert.ErrorIs(t, g.StartProject(ctx, p), ErrWorkloadLatched)
	assert.ErrorIs(t, g.RecoverServices(ctx, p, []string{"web"}), ErrWorkloadLatched)
	assert.ErrorIs(t, g.UpdateServices(ctx, p, dockerSpecDrift()), ErrWorkloadLatched)
}

func TestNilStopLatchesGuardNothing(t *testing.T) {
	var latches *StopLatches
	p := latchedAssignment("blog", "uid-1", 4)
	assert.NoError(t, latches.Record(p, time.Now()))
	held, err := latches.Holds(p)
	require.NoError(t, err)
	assert.False(t, held)
	assert.NoError(t, latches.refuseStart(p))
	latches.Sync([]*v1.Project{p}, time.Now(), zap.NewNop())
}

func dockerSpecDrift() docker.SpecDrift { return docker.SpecDrift{} }
