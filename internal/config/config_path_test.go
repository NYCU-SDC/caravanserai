package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveConfigPath(t *testing.T) {
	t.Run("flag beats the environment", func(t *testing.T) {
		t.Setenv("CARA_CONFIG", "/from/env.yaml")
		for _, args := range [][]string{
			{"--config", "/from/flag.yaml"},
			{"--config=/from/flag.yaml"},
			{"-config", "/from/flag.yaml", "--agent-port", "1"},
		} {
			path, explicit := resolveConfigPath(args)
			assert.Equal(t, "/from/flag.yaml", path)
			assert.True(t, explicit)
		}
	})

	t.Run("environment beats the defaults", func(t *testing.T) {
		t.Setenv("CARA_CONFIG", "/from/env.yaml")
		path, explicit := resolveConfigPath(nil)
		assert.Equal(t, "/from/env.yaml", path)
		assert.True(t, explicit)
	})

	t.Run("a file in the working directory is a default, not explicit", func(t *testing.T) {
		t.Setenv("CARA_CONFIG", "")
		dir := t.TempDir()
		t.Chdir(dir)
		assert.NoError(t, os.WriteFile(filepath.Join(dir, localConfigPath), nil, 0o600))
		path, explicit := resolveConfigPath(nil)
		assert.Equal(t, localConfigPath, path)
		assert.False(t, explicit)
	})
}

func TestExplicitFileError(t *testing.T) {
	missing := errors.New("no such file")
	assert.Error(t, explicitFileError(true, missing), "a named file that cannot be used must not be ignored")
	assert.NoError(t, explicitFileError(false, missing), "an absent default is normal")
	assert.NoError(t, explicitFileError(true, nil))
}
