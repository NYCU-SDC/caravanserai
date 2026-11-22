package config

import (
	"testing"
	"time"

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

func TestServerFromEnvLoadsTerminatingTimeout(t *testing.T) {
	t.Setenv("TERMINATING_TIMEOUT", "45s")

	got, err := FromEnv(&Config{}, NewConfigLogger())
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, got.TerminatingTimeout)

	t.Setenv("TERMINATING_TIMEOUT", "soon")
	got, err = FromEnv(&Config{}, NewConfigLogger())
	require.NoError(t, err)
	assert.Zero(t, got.TerminatingTimeout, "an unparsable value is ignored, leaving the default")
}

func TestServerConfig_Validate_TerminatingTimeout(t *testing.T) {
	cfg := Config{DatabaseURL: "postgres://localhost/cara", TerminatingTimeout: -time.Second}
	assert.Error(t, cfg.Validate())

	cfg.TerminatingTimeout = 0
	assert.NoError(t, cfg.Validate(), "zero means the default")
}

func TestServerFromEnvLoadsThePostgresDifferentialImage(t *testing.T) {
	t.Setenv("POSTGRES_DIFFERENTIAL_IMAGE", "registry.example/cara-postgres:16")
	got, err := FromEnv(&Config{}, NewConfigLogger())
	require.NoError(t, err)
	assert.Equal(t, "registry.example/cara-postgres:16", got.PostgresDifferentialImage)
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

func TestServerConfig_Validate_HeadscaleAPI(t *testing.T) {
	base := func() Config {
		return Config{DatabaseURL: "postgres://localhost/cara"}
	}

	tests := []struct {
		name    string
		apiURL  string
		apiKey  string
		wantErr bool
	}{
		{
			name: "overlay admin disabled: both empty",
		},
		{
			name:   "overlay admin enabled: both set",
			apiURL: "http://localhost:8081",
			apiKey: "secret",
		},
		{
			name:    "half-configured: only URL set",
			apiURL:  "http://localhost:8081",
			wantErr: true,
		},
		{
			name:    "half-configured: only key set",
			apiKey:  "secret",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			cfg.HeadscaleAPIURL = tt.apiURL
			cfg.HeadscaleAPIKey = tt.apiKey

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

func TestServerFromEnvLoadsLeaseFields(t *testing.T) {
	t.Setenv("LEASE_S3_ENDPOINT", "https://r2.example")
	t.Setenv("LEASE_S3_BUCKET", "cara-lease")
	t.Setenv("LEASE_S3_REGION", "auto")
	t.Setenv("LEASE_S3_ACCESS_KEY", "id")
	t.Setenv("LEASE_S3_SECRET_KEY", "secret")
	t.Setenv("LEASE_S3_CA_FILE", "/etc/cara/lease-ca.crt")
	t.Setenv("LEASE_S3_KEY", "custom/lease.json")
	t.Setenv("SERVER_ID", "cp-1")
	t.Setenv("SERVER_ADVERTISE_URL", "http://10.0.0.11:8080")

	got, err := FromEnv(&Config{}, NewConfigLogger())
	require.NoError(t, err)
	assert.True(t, got.LeaseEnabled())
	assert.Equal(t, S3Config{
		Endpoint: "https://r2.example", Bucket: "cara-lease", Region: "auto",
		AccessKey: "id", SecretKey: "secret", CAFile: "/etc/cara/lease-ca.crt",
	}, got.LeaseS3())
	assert.Equal(t, "custom/lease.json", got.LeaseS3Key)
	assert.Equal(t, "cp-1", got.ServerID)
	assert.Equal(t, "http://10.0.0.11:8080", got.AdvertiseURL)
}

func TestServerConfig_Validate_Lease(t *testing.T) {
	full := func() Config {
		return Config{
			DatabaseURL:      "postgres://localhost/cara",
			LeaseS3Endpoint:  "https://r2.example",
			LeaseS3Bucket:    "cara-lease",
			LeaseS3AccessKey: "id",
			LeaseS3SecretKey: "secret",
			AdvertiseURL:     "http://10.0.0.11:8080",
		}
	}
	tests := []struct {
		name    string
		edit    func(*Config)
		wantErr string
	}{
		{name: "no lease is the single-server default", edit: func(c *Config) { *c = Config{DatabaseURL: "postgres://localhost/cara"} }},
		{name: "a complete lease", edit: func(*Config) {}},
		{name: "a bucket without an endpoint", edit: func(c *Config) { c.LeaseS3Endpoint = "" }, wantErr: "lease_s3_endpoint is required"},
		{name: "an endpoint without a scheme", edit: func(c *Config) { c.LeaseS3Endpoint = "r2.example" }, wantErr: "must start with http:// or https://"},
		{name: "no bucket", edit: func(c *Config) { c.LeaseS3Bucket = "" }, wantErr: "bucket is required"},
		{name: "no secret key", edit: func(c *Config) { c.LeaseS3SecretKey = "" }, wantErr: "secret_key is required"},
		{name: "no advertise URL", edit: func(c *Config) { c.AdvertiseURL = "" }, wantErr: "advertise_url is required"},
		{name: "an advertise URL without a scheme", edit: func(c *Config) { c.AdvertiseURL = "10.0.0.11:8080" }, wantErr: "absolute http:// or https:// URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full()
			tt.edit(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestServerConfig_ResolvedServerID(t *testing.T) {
	got, err := (&Config{ServerID: "cp-1"}).ResolvedServerID()
	require.NoError(t, err)
	assert.Equal(t, "cp-1", got)

	got, err = (&Config{}).ResolvedServerID()
	require.NoError(t, err)
	assert.NotEmpty(t, got, "falls back to the hostname")
}
