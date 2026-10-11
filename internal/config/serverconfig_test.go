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

func TestServerMaxInFlightEvictions(t *testing.T) {
	t.Run("loaded from the environment", func(t *testing.T) {
		t.Setenv("MAX_INFLIGHT_EVICTIONS", "2")
		got, err := FromEnv(&Config{}, NewConfigLogger())
		require.NoError(t, err)
		assert.Equal(t, 2, got.MaxInFlightEvictions)
	})

	t.Run("unset leaves the default to the controller", func(t *testing.T) {
		t.Setenv("MAX_INFLIGHT_EVICTIONS", "")
		got, err := FromEnv(&Config{}, NewConfigLogger())
		require.NoError(t, err)
		assert.Zero(t, got.MaxInFlightEvictions)
	})

	t.Run("not a number is an error", func(t *testing.T) {
		t.Setenv("MAX_INFLIGHT_EVICTIONS", "one")
		_, err := FromEnv(&Config{}, NewConfigLogger())
		assert.ErrorContains(t, err, "MAX_INFLIGHT_EVICTIONS")
	})

	t.Run("negative is rejected", func(t *testing.T) {
		cfg := Config{DatabaseURL: "postgres://localhost/cara", MaxInFlightEvictions: -1}
		assert.ErrorContains(t, cfg.Validate(), "max_inflight_evictions")
	})

	t.Run("zero and positive are valid", func(t *testing.T) {
		for _, n := range []int{0, 1, 3} {
			cfg := Config{DatabaseURL: "postgres://localhost/cara", MaxInFlightEvictions: n}
			assert.NoError(t, cfg.Validate(), "max_inflight_evictions=%d", n)
		}
	})
}

func TestServerFailbackWindow(t *testing.T) {
	t.Run("unset disables fail-back", func(t *testing.T) {
		cfg := Config{DatabaseURL: "postgres://localhost/cara"}
		w, err := cfg.FailbackDailyWindow()
		require.NoError(t, err)
		assert.Nil(t, w)
		assert.NoError(t, cfg.Validate())
	})

	t.Run("read in Taiwan time by default", func(t *testing.T) {
		cfg := Config{FailbackWindow: "02:00-05:00"}
		w, err := cfg.FailbackDailyWindow()
		require.NoError(t, err)
		require.NotNil(t, w)
		assert.True(t, w.Contains(time.Date(2026, 10, 9, 18, 30, 0, 0, time.UTC)), "02:30 Taiwan")
		assert.Contains(t, w.Describe(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)), "Asia/Taipei (18:00-21:00 UTC)")
	})

	t.Run("another time zone", func(t *testing.T) {
		cfg := Config{FailbackWindow: "02:00-05:00", FailbackTimezone: "UTC"}
		w, err := cfg.FailbackDailyWindow()
		require.NoError(t, err)
		assert.True(t, w.Contains(time.Date(2026, 10, 10, 2, 30, 0, 0, time.UTC)))
	})

	t.Run("a malformed window fails validation", func(t *testing.T) {
		cfg := Config{DatabaseURL: "postgres://localhost/cara", FailbackWindow: "2am-5am"}
		assert.ErrorContains(t, cfg.Validate(), "failback_window")
	})

	t.Run("an unknown time zone fails validation", func(t *testing.T) {
		cfg := Config{DatabaseURL: "postgres://localhost/cara", FailbackWindow: "02:00-05:00", FailbackTimezone: "Taiwan/Hsinchu"}
		assert.ErrorContains(t, cfg.Validate(), "failback_window")
	})

	t.Run("loaded from the environment", func(t *testing.T) {
		t.Setenv("FAILBACK_WINDOW", "22:00-02:00")
		t.Setenv("FAILBACK_TIMEZONE", "Asia/Tokyo")
		got, err := FromEnv(&Config{}, NewConfigLogger())
		require.NoError(t, err)
		assert.Equal(t, "22:00-02:00", got.FailbackWindow)
		assert.Equal(t, "Asia/Tokyo", got.FailbackTimezone)
	})
}

func TestServerFailbackPolicySettings(t *testing.T) {
	base := func() Config { return Config{DatabaseURL: "postgres://localhost/cara"} }

	t.Run("unset is valid and left to the defaults", func(t *testing.T) {
		c := base()
		assert.NoError(t, c.Validate())
	})

	t.Run("loaded from the environment", func(t *testing.T) {
		t.Setenv("FAILBACK_STABLE_WINDOW", "45m")
		t.Setenv("FAILBACK_FLAP_WINDOW", "48h")
		t.Setenv("FAILBACK_BACKOFF_MULTIPLIER", "3")
		t.Setenv("FAILBACK_MAX_STABLE_WINDOW", "12h")
		t.Setenv("FAILBACK_COOLDOWN", "2h")
		t.Setenv("FAILBACK_MIN_FREE_PERCENT", "15.5")
		got, err := FromEnv(&Config{}, NewConfigLogger())
		require.NoError(t, err)
		assert.Equal(t, 45*time.Minute, got.FailbackStableWindow)
		assert.Equal(t, 48*time.Hour, got.FailbackFlapWindow)
		assert.Equal(t, 3, got.FailbackBackoffMultiplier)
		assert.Equal(t, 12*time.Hour, got.FailbackMaxStableWindow)
		assert.Equal(t, 2*time.Hour, got.FailbackCooldown)
		assert.Equal(t, 15.5, got.FailbackMinFreePercent)
	})

	t.Run("a malformed duration is an error", func(t *testing.T) {
		t.Setenv("FAILBACK_COOLDOWN", "six hours")
		_, err := FromEnv(&Config{}, NewConfigLogger())
		assert.ErrorContains(t, err, "FAILBACK_COOLDOWN")
	})

	invalid := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"negative stable window", func(c *Config) { c.FailbackStableWindow = -time.Minute }, "failback_stable_window"},
		{"negative cooldown", func(c *Config) { c.FailbackCooldown = -time.Hour }, "failback_cooldown"},
		{"flap window longer than the history kept", func(c *Config) { c.FailbackFlapWindow = 8 * 24 * time.Hour }, "failback_flap_window"},
		{"negative multiplier", func(c *Config) { c.FailbackBackoffMultiplier = -1 }, "failback_backoff_multiplier"},
		{"percent over 100", func(c *Config) { c.FailbackMinFreePercent = 101 }, "failback_min_free_percent"},
		{"negative percent", func(c *Config) { c.FailbackMinFreePercent = -1 }, "failback_min_free_percent"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(&c)
			assert.ErrorContains(t, c.Validate(), tt.want)
		})
	}

	t.Run("a seven day flap window is the most allowed", func(t *testing.T) {
		c := base()
		c.FailbackFlapWindow = 7 * 24 * time.Hour
		assert.NoError(t, c.Validate())
	})
}
