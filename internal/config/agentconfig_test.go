package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentConfig_Validate_Overlay(t *testing.T) {
	base := func() AgentConfig {
		return AgentConfig{ServerURL: "http://localhost:8080", AdvertiseIP: "10.0.0.1"}
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
