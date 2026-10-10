package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunForEviction(t *testing.T) {
	t.Run("a committed backup leaves the containers stopped while the move is wanted", func(t *testing.T) {
		h := newHarness(t, "db-data")
		var asked int
		backupID, leftStopped, err := h.runner.RunForEviction(context.Background(), h.project,
			func(context.Context) bool { asked++; return true })

		require.NoError(t, err)
		assert.True(t, leftStopped)
		assert.NotEmpty(t, backupID)
		assert.Equal(t, 1, asked)
		assert.Equal(t, []string{"stop"}, h.containers.history(), "no restart: the move takes it from here")
		assert.Equal(t, 0, h.routes.count())
		assert.Equal(t, []string{"set", "clear"}, h.conditions.history())
		assert.Contains(t, h.store.keys()[len(h.store.keys())-1], "latest.json", "the generation is complete")
	})

	t.Run("a move called off meanwhile restarts the containers", func(t *testing.T) {
		h := newHarness(t, "db-data")
		backupID, leftStopped, err := h.runner.RunForEviction(context.Background(), h.project,
			func(context.Context) bool { return false })

		require.NoError(t, err)
		assert.False(t, leftStopped)
		assert.NotEmpty(t, backupID)
		assert.Equal(t, []string{"stop", "start"}, h.containers.history())
	})

	t.Run("a failed backup restarts the containers without asking", func(t *testing.T) {
		h := newHarness(t, "db-data")
		h.store.failOnKey = "manifest.json"
		var asked bool
		backupID, leftStopped, err := h.runner.RunForEviction(context.Background(), h.project,
			func(context.Context) bool { asked = true; return true })

		require.Error(t, err)
		assert.False(t, leftStopped)
		assert.Empty(t, backupID)
		assert.False(t, asked)
		assert.Equal(t, []string{"stop", "start"}, h.containers.history())
	})

	t.Run("a busy project is reported, not skipped silently", func(t *testing.T) {
		h := newHarness(t, "db-data")
		_, ok := h.runner.coordinator.TryClaim(ResourceKey{Namespace: "default", Name: "blog"}, OpBackup)
		require.True(t, ok)

		_, leftStopped, err := h.runner.RunForEviction(context.Background(), h.project,
			func(context.Context) bool { return true })

		assert.ErrorIs(t, err, ErrProjectBusy)
		assert.False(t, leftStopped)
		assert.Empty(t, h.containers.history())
	})

	t.Run("Run keeps restarting after a backup", func(t *testing.T) {
		h := newHarness(t, "db-data")
		require.NoError(t, h.runner.Run(context.Background(), h.project))
		assert.Equal(t, []string{"stop", "start"}, h.containers.history())
	})
}
