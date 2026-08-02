package overlay

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestReadPreauthKey(t *testing.T) {
	dir := t.TempDir()

	validPath := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(validPath, []byte("  hskey-auth-abc123\n"), 0o600))

	emptyPath := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(emptyPath, []byte("   \n"), 0o600))

	tests := []struct {
		name       string
		path       string
		wantKey    string
		wantConfig bool // expect an ErrConfig-wrapped error
	}{
		{name: "valid key is trimmed", path: validPath, wantKey: "hskey-auth-abc123"},
		{name: "missing file", path: filepath.Join(dir, "nope"), wantConfig: true},
		{name: "empty file", path: emptyPath, wantConfig: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := readPreauthKey(tt.path)
			if tt.wantConfig {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConfig)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, key)
		})
	}
}

func TestNewTsnetClient_ConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     TsnetConfig
		wantErr bool
	}{
		{
			name: "complete config",
			cfg:  TsnetConfig{ControlURL: "http://localhost:8081", PreauthKeyFile: "/tmp/key", Hostname: "node-1"},
		},
		{
			name:    "missing control URL",
			cfg:     TsnetConfig{PreauthKeyFile: "/tmp/key", Hostname: "node-1"},
			wantErr: true,
		},
		{
			name:    "missing key file",
			cfg:     TsnetConfig{ControlURL: "http://localhost:8081", Hostname: "node-1"},
			wantErr: true,
		},
		{
			name:    "missing hostname",
			cfg:     TsnetConfig{ControlURL: "http://localhost:8081", PreauthKeyFile: "/tmp/key"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewTsnetClient(tt.cfg, zap.NewNop())
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConfig)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, c)
		})
	}
}

// Close must be a no-op (not panic) when Join never ran.
func TestTsnetClient_CloseBeforeJoin(t *testing.T) {
	c, err := NewTsnetClient(TsnetConfig{
		ControlURL:     "http://localhost:8081",
		PreauthKeyFile: "/tmp/key",
		Hostname:       "node-1",
	}, zap.NewNop())
	require.NoError(t, err)
	assert.NoError(t, c.Close())
	assert.Empty(t, c.OverlayIP())
}
