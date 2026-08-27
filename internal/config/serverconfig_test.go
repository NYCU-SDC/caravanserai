package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerFromEnvLoadsOverlayFields(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cara")
	t.Setenv("HEADSCALE_URL", "http://localhost:8081")
	t.Setenv("HEADSCALE_PREAUTH_KEY_FILE", "/etc/cara/preauth.key")
	t.Setenv("OVERLAY_HOSTNAME", "cara-server-1")
	t.Setenv("OVERLAY_STATE_DIR", "/var/lib/cara-server/tsnet")

	got, err := FromEnv(&Config{}, NewConfigLogger())
	require.NoError(t, err)

	assert.Equal(t, "http://localhost:8081", got.HeadscaleURL)
	assert.Equal(t, "/etc/cara/preauth.key", got.PreauthKeyFile)
	assert.Equal(t, "cara-server-1", got.OverlayHostname)
	assert.Equal(t, "/var/lib/cara-server/tsnet", got.OverlayStateDir)
}

func TestServerConfig_Validate_Overlay(t *testing.T) {
	base := func() Config {
		return Config{DatabaseURL: "postgres://localhost/cara"}
	}

	tests := []struct {
		name           string
		headscaleURL   string
		preauthKeyFile string
		wantErr        bool
	}{
		{
			name: "overlay disabled: both empty",
		},
		{
			name:           "overlay enabled: both set",
			headscaleURL:   "http://localhost:8081",
			preauthKeyFile: "/etc/cara/preauth.key",
		},
		{
			name:         "half-configured: only URL set",
			headscaleURL: "http://localhost:8081",
			wantErr:      true,
		},
		{
			name:           "half-configured: only key file set",
			preauthKeyFile: "/etc/cara/preauth.key",
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			cfg.HeadscaleURL = tt.headscaleURL
			cfg.PreauthKeyFile = tt.preauthKeyFile

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestServerConfig_Validate_RequiresDatabaseURL(t *testing.T) {
	err := (&Config{}).Validate()
	assert.ErrorContains(t, err, "database_url is required")
}
