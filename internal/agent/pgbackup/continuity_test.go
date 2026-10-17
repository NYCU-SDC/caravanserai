package pgbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLogs is a container log that grows, rotates and fails on demand.
type fakeLogs struct {
	lines []string
	n     int
	err   error
}

func (f *fakeLogs) add(texts ...string) {
	for _, t := range texts {
		f.n++
		f.lines = append(f.lines, fmt.Sprintf("2026-10-01T03:00:%02d.000000000Z %s", f.n, t))
	}
}

func (f *fakeLogs) LogLines(context.Context, string) ([]string, error) {
	return append([]string(nil), f.lines...), f.err
}

func testDB() Database {
	return Database{ProjectUID: "uid-1", Volume: "pgdata", Service: "db", Stanza: "uid-1-pgdata",
		ContainerID: "cid-1", SystemID: "7400", Timeline: "00000001"}
}

const dropped = "WARN: dropped WAL file '000000010000000000000007' because archive queue exceeded 4GB"

// continuousAfterFull is a continuity whose latch a full has already cleared,
// with log lines observed so far.
func continuousAfterFull(t *testing.T) (*FileContinuity, *fakeLogs, uint64) {
	t.Helper()
	logs := &fakeLogs{}
	logs.add("database system is ready to accept connections")
	c := NewFileContinuity(t.TempDir(), logs)
	known, v, err := c.Check(context.Background(), testDB())
	require.NoError(t, err)
	require.False(t, known, "a database never observed has unknown continuity")
	require.NoError(t, c.FullAccepted(context.Background(), testDB(), v))
	known, v, err = c.Check(context.Background(), testDB())
	require.NoError(t, err)
	require.True(t, known)
	return c, logs, v
}

func TestContinuityStaysKnownWhileTheLogContinues(t *testing.T) {
	c, logs, v := continuousAfterFull(t)
	logs.add("checkpoint starting: time", "checkpoint complete")
	known, v2, err := c.Check(context.Background(), testDB())
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, v, v2)
}

func TestDroppedWALLatchesUntilAFullFromThatVersion(t *testing.T) {
	c, logs, v := continuousAfterFull(t)
	ctx := context.Background()
	logs.add(dropped)

	known, gapVersion, err := c.Check(ctx, testDB())
	require.NoError(t, err)
	assert.False(t, known)
	assert.Greater(t, gapVersion, v)

	// An Agent restart reads the same file.
	restarted := NewFileContinuity(c.dir[:len(c.dir)-len("/pgbackrest/continuity")], logs)
	known, _, err = restarted.Check(ctx, testDB())
	require.NoError(t, err)
	assert.False(t, known, "the latch survives a restart")

	// A full that started before the gap does not clear it.
	require.NoError(t, restarted.FullAccepted(ctx, testDB(), v))
	known, _, _ = restarted.Check(ctx, testDB())
	assert.False(t, known)

	require.NoError(t, restarted.FullAccepted(ctx, testDB(), gapVersion))
	known, _, _ = restarted.Check(ctx, testDB())
	assert.True(t, known)
}

// A gap logged while the full ran is seen when the full is accepted, so that
// full does not clear it.
func TestAGapDuringTheFullKeepsTheLatch(t *testing.T) {
	c, logs, _ := continuousAfterFull(t)
	ctx := context.Background()
	logs.add(dropped)
	_, started, err := c.Check(ctx, testDB())
	require.NoError(t, err)

	logs.add(dropped) // during the full
	require.NoError(t, c.FullAccepted(ctx, testDB(), started))

	known, _, err := c.Check(ctx, testDB())
	require.NoError(t, err)
	assert.False(t, known)
}

func TestContinuityIsUnknownWhenItCannotBeProven(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*FileContinuity, *fakeLogs, *Database)
	}{
		{"latch file missing", func(c *FileContinuity, _ *fakeLogs, db *Database) { require.NoError(t, os.Remove(c.path(*db))) }},
		{"latch file unreadable", func(c *FileContinuity, _ *fakeLogs, db *Database) {
			require.NoError(t, os.WriteFile(c.path(*db), []byte("{"), 0o600))
		}},
		{"container replaced", func(_ *FileContinuity, _ *fakeLogs, db *Database) { db.ContainerID = "cid-2" }},
		{"new timeline", func(_ *FileContinuity, _ *fakeLogs, db *Database) { db.Timeline = "00000002" }},
		{"other data history", func(_ *FileContinuity, _ *fakeLogs, db *Database) { db.SystemID = "7401" }},
		{"log truncated", func(_ *FileContinuity, l *fakeLogs, _ *Database) { l.lines = nil; l.add("after truncation") }},
		{"log rotated", func(_ *FileContinuity, l *fakeLogs, _ *Database) {
			l.add("a", "b")
			l.lines = l.lines[2:]
		}},
		{"log unreadable", func(_ *FileContinuity, l *fakeLogs, _ *Database) { l.err = errors.New("logging driver none") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, logs, v := continuousAfterFull(t)
			db := testDB()
			tc.break_(c, logs, &db)
			known, v2, _ := c.Check(context.Background(), db)
			assert.False(t, known)
			if tc.name != "latch file missing" && tc.name != "latch file unreadable" {
				assert.Greater(t, v2, v, "the version never goes back")
			}
			// Unknown stays unknown until a full clears it, even once the log
			// reads again.
			logs.err = nil
			known, _, _ = c.Check(context.Background(), db)
			assert.False(t, known)
		})
	}
}

func TestLatchFileIsPrivateToTheAgent(t *testing.T) {
	c, _, _ := continuousAfterFull(t)
	info, err := os.Stat(c.path(testDB()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(c.path(testDB())))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
}
