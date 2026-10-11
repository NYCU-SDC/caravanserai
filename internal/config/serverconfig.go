package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"NYCU-SDC/caravanserai/internal/dailywindow"

	configutil "github.com/NYCU-SDC/summer/pkg/config"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Config holds the runtime configuration for cara-server (control plane).
type Config struct {
	Debug            bool   `yaml:"debug"              envconfig:"DEBUG"`
	Host             string `yaml:"host"               envconfig:"HOST"`
	Port             string `yaml:"port"               envconfig:"PORT"`
	OtelCollectorUrl string `yaml:"otel_collector_url" envconfig:"OTEL_COLLECTOR_URL"`
	DatabaseURL      string `yaml:"database_url"       envconfig:"DATABASE_URL"`

	// Overlay networking is opt-in in 1.0 (CARA-74): cara-server joins the
	// Headscale mesh on startup only when both HeadscaleURL and PreauthKeyFile
	// are set, so that server→agent calls can reach agents on their overlay
	// IPs.  When both are empty the server runs on the underlay as before.
	HeadscaleURL string `yaml:"headscale_url" envconfig:"HEADSCALE_URL"`
	// PreauthKeyFile is the path to a file containing the Headscale pre-auth
	// key used to join the overlay.  The key itself is never logged.
	PreauthKeyFile string `yaml:"preauth_key_file" envconfig:"HEADSCALE_PREAUTH_KEY_FILE"`
	// OverlayHostname optionally overrides the hostname the server registers
	// with Headscale.  Defaults to "cara-server" when empty.
	OverlayHostname string `yaml:"overlay_hostname" envconfig:"OVERLAY_HOSTNAME"`
	// OverlayStateDir optionally overrides where tsnet persists node keys.
	// Defaults to a server-specific directory so it never collides with a
	// co-located cara-agent's tsnet state.
	OverlayStateDir string `yaml:"overlay_state_dir" envconfig:"OVERLAY_STATE_DIR"`

	// Headscale management API access (CARA-49). When both HeadscaleAPIURL and
	// HeadscaleAPIKey are set, cara-server exposes the /api/v1/overlay endpoints
	// (issue pre-auth keys, list/revoke nodes). When unset those endpoints
	// report that overlay administration is not configured. The API key is
	// secret and is never logged.
	HeadscaleAPIURL string `yaml:"headscale_api_url" envconfig:"HEADSCALE_API_URL"`
	HeadscaleAPIKey string `yaml:"headscale_api_key" envconfig:"HEADSCALE_API_KEY"`
	// HeadscaleUser is the Headscale user new pre-auth keys are created under.
	// Defaults to "cara-node".
	HeadscaleUser string `yaml:"headscale_user" envconfig:"HEADSCALE_USER"`

	// UIDEnforcement turns on strict UID and assignment-generation fencing on
	// the server side (CARA-82, CARA-83). It shares the agent's UID_ENFORCEMENT
	// switch on purpose: UID and generation are one ownership identity, rolled
	// out together. When false (the default, "compatibility mode") an Agent
	// report that carries a complete UID + nodeRef + generation fence is still
	// validated atomically, but a legacy report missing that fence is accepted
	// through the unfenced path so an un-upgraded Agent keeps working. When
	// true, a report missing UID or generation is rejected as a stale
	// assignment rather than accepted. Enable it cluster-wide only after every
	// Agent sends the fence and legacy runtime ownership has been remediated.
	UIDEnforcement bool `yaml:"uid_enforcement" envconfig:"UID_ENFORCEMENT"`

	// MaxInFlightEvictions caps how many planned moves (drain, fail-back) run
	// at once across the cluster (CARA-119). Each move is downtime plus a
	// backup and a restore, so the default is one. Zero means the default.
	MaxInFlightEvictions int `yaml:"max_inflight_evictions" envconfig:"MAX_INFLIGHT_EVICTIONS"`

	// FailbackWindow is the daily window, "HH:MM-HH:MM" in FailbackTimezone,
	// in which Projects on the backup tier are moved back to primary
	// (CARA-121), e.g. "02:00-05:00". It may cross midnight. Empty disables
	// fail-back.
	FailbackWindow string `yaml:"failback_window" envconfig:"FAILBACK_WINDOW"`
	// FailbackTimezone is the IANA time zone FailbackWindow is read in.
	// Defaults to DefaultFailbackTimezone.
	FailbackTimezone string `yaml:"failback_timezone" envconfig:"FAILBACK_TIMEZONE"`

	// Fail-back's defences against a flapping primary tier (CARA-122). Zero
	// means the default; see controller.DefaultFailbackPolicy.
	//
	// FailbackStableWindow is how long a primary node must have been Ready
	// before it may take a Project back. Each NotReady within
	// FailbackFlapWindow beyond the first multiplies it by
	// FailbackBackoffMultiplier, up to FailbackMaxStableWindow.
	FailbackStableWindow      time.Duration `yaml:"failback_stable_window"      envconfig:"FAILBACK_STABLE_WINDOW"`
	FailbackFlapWindow        time.Duration `yaml:"failback_flap_window"        envconfig:"FAILBACK_FLAP_WINDOW"`
	FailbackBackoffMultiplier int           `yaml:"failback_backoff_multiplier" envconfig:"FAILBACK_BACKOFF_MULTIPLIER"`
	FailbackMaxStableWindow   time.Duration `yaml:"failback_max_stable_window"  envconfig:"FAILBACK_MAX_STABLE_WINDOW"`
	// FailbackCooldown is how long a Project must have run where it is before
	// fail-back moves it.
	FailbackCooldown time.Duration `yaml:"failback_cooldown" envconfig:"FAILBACK_COOLDOWN"`
	// FailbackMinFreePercent is how much of a primary node's CPU and memory
	// must still be free once the Project is placed there.
	FailbackMinFreePercent float64 `yaml:"failback_min_free_percent" envconfig:"FAILBACK_MIN_FREE_PERCENT"`
}

// DefaultFailbackTimezone is the time zone FailbackWindow is read in when
// FailbackTimezone is not set. Taiwan keeps UTC+8 all year.
const DefaultFailbackTimezone = "Asia/Taipei"

// FailbackDailyWindow returns the configured fail-back window, or nil when
// fail-back is disabled.
func (c *Config) FailbackDailyWindow() (*dailywindow.Window, error) {
	if c.FailbackWindow == "" {
		return nil, nil
	}
	tz := c.FailbackTimezone
	if tz == "" {
		tz = DefaultFailbackTimezone
	}
	w, err := dailywindow.Parse(c.FailbackWindow, tz)
	if err != nil {
		return nil, fmt.Errorf("failback_window: %w", err)
	}
	return &w, nil
}

// Load reads cara-server config from file → env → flags (later sources win).
func Load() (Config, *LogBuffer) {
	logger := NewConfigLogger()

	cfg := &Config{
		Debug: false,
		Host:  "0.0.0.0",
		Port:  "8080",
	}

	var err error

	cfg, err = FromFile("config.yaml", cfg, logger)
	if err != nil {
		logger.Warn("Failed to load config from file", err, map[string]string{"path": "config.yaml"})
	}

	cfg, err = FromEnv(cfg, logger)
	if err != nil {
		logger.Warn("Failed to load config from env", err, map[string]string{"path": ".env"})
	}

	cfg, err = FromFlags(cfg)
	if err != nil {
		logger.Warn("Failed to load config from flags", err, nil)
	}

	return *cfg, logger
}

func FromFile(filePath string, cfg *Config, logger *LogBuffer) (*Config, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return cfg, err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			logger.Warn("Failed to close config file", cerr, map[string]string{"path": filePath})
		}
	}()

	fileConfig := Config{}
	if err := yaml.NewDecoder(file).Decode(&fileConfig); err != nil {
		return cfg, err
	}

	return configutil.Merge[Config](cfg, &fileConfig)
}

func FromEnv(cfg *Config, logger *LogBuffer) (*Config, error) {
	if err := godotenv.Overload(); err != nil {
		if os.IsNotExist(err) {
			logger.Warn("No .env file found", err, map[string]string{"path": ".env"})
		} else {
			return nil, err
		}
	}

	envConfig := &Config{
		Debug:            os.Getenv("DEBUG") == "true",
		Host:             os.Getenv("HOST"),
		Port:             os.Getenv("PORT"),
		OtelCollectorUrl: os.Getenv("OTEL_COLLECTOR_URL"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		HeadscaleURL:     os.Getenv("HEADSCALE_URL"),
		PreauthKeyFile:   os.Getenv("HEADSCALE_PREAUTH_KEY_FILE"),
		OverlayHostname:  os.Getenv("OVERLAY_HOSTNAME"),
		OverlayStateDir:  os.Getenv("OVERLAY_STATE_DIR"),
		HeadscaleAPIURL:  os.Getenv("HEADSCALE_API_URL"),
		HeadscaleAPIKey:  os.Getenv("HEADSCALE_API_KEY"),
		HeadscaleUser:    os.Getenv("HEADSCALE_USER"),
		UIDEnforcement:   os.Getenv("UID_ENFORCEMENT") == "true",
		FailbackWindow:   os.Getenv("FAILBACK_WINDOW"),
		FailbackTimezone: os.Getenv("FAILBACK_TIMEZONE"),
	}
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{
		{"FAILBACK_STABLE_WINDOW", &envConfig.FailbackStableWindow},
		{"FAILBACK_FLAP_WINDOW", &envConfig.FailbackFlapWindow},
		{"FAILBACK_MAX_STABLE_WINDOW", &envConfig.FailbackMaxStableWindow},
		{"FAILBACK_COOLDOWN", &envConfig.FailbackCooldown},
	} {
		if v := os.Getenv(d.env); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.env, err)
			}
			*d.dst = parsed
		}
	}
	if v := os.Getenv("FAILBACK_BACKOFF_MULTIPLIER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("FAILBACK_BACKOFF_MULTIPLIER: %w", err)
		}
		envConfig.FailbackBackoffMultiplier = n
	}
	if v := os.Getenv("FAILBACK_MIN_FREE_PERCENT"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("FAILBACK_MIN_FREE_PERCENT: %w", err)
		}
		envConfig.FailbackMinFreePercent = f
	}
	if v := os.Getenv("MAX_INFLIGHT_EVICTIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("MAX_INFLIGHT_EVICTIONS: %w", err)
		}
		envConfig.MaxInFlightEvictions = n
	}

	return configutil.Merge[Config](cfg, envConfig)
}

func FromFlags(cfg *Config) (*Config, error) {
	flagConfig := &Config{}
	flag.BoolVar(&flagConfig.Debug, "debug", false, "enable debug mode")
	flag.StringVar(&flagConfig.Host, "host", "", "listen host")
	flag.StringVar(&flagConfig.Port, "port", "", "listen port")
	flag.StringVar(&flagConfig.OtelCollectorUrl, "otel_collector_url", "", "OpenTelemetry collector URL")
	flag.StringVar(&flagConfig.DatabaseURL, "database_url", "", "PostgreSQL connection URL")
	flag.StringVar(&flagConfig.HeadscaleURL, "headscale-url", "", "Headscale control-plane URL to join on startup (enables overlay networking)")
	flag.StringVar(&flagConfig.PreauthKeyFile, "preauth-key-file", "", "path to a file containing the Headscale pre-auth key")
	flag.StringVar(&flagConfig.OverlayHostname, "overlay-hostname", "", "hostname to register with Headscale (default: cara-server)")
	flag.StringVar(&flagConfig.OverlayStateDir, "overlay-state-dir", "", "directory where tsnet persists overlay node keys (default: per-user cara-server dir)")
	flag.StringVar(&flagConfig.HeadscaleAPIURL, "headscale-api-url", "", "Headscale management API URL (enables overlay admin endpoints)")
	flag.StringVar(&flagConfig.HeadscaleAPIKey, "headscale-api-key", "", "Headscale management API key")
	flag.StringVar(&flagConfig.HeadscaleUser, "headscale-user", "", "Headscale user new pre-auth keys are created under (default: cara-node)")
	flag.BoolVar(&flagConfig.UIDEnforcement, "uid-enforcement", false, "enforce Project UID and assignment-generation fencing (default: false, compatibility mode)")
	flag.IntVar(&flagConfig.MaxInFlightEvictions, "max-inflight-evictions", 0, "planned moves (drain, fail-back) allowed at once across the cluster (default: 1)")
	flag.StringVar(&flagConfig.FailbackWindow, "failback-window", "", "daily window HH:MM-HH:MM in which projects are moved back to the primary tier (default: disabled)")
	flag.StringVar(&flagConfig.FailbackTimezone, "failback-timezone", "", "IANA time zone the fail-back window is read in (default: Asia/Taipei)")
	flag.DurationVar(&flagConfig.FailbackStableWindow, "failback-stable-window", 0, "how long a primary node must have been Ready before fail-back uses it (default: 30m)")
	flag.DurationVar(&flagConfig.FailbackFlapWindow, "failback-flap-window", 0, "how far back a primary node's NotReady transitions are counted (default: 24h, at most 168h)")
	flag.IntVar(&flagConfig.FailbackBackoffMultiplier, "failback-backoff-multiplier", 0, "stable window multiplier per extra NotReady in the flap window (default: 2)")
	flag.DurationVar(&flagConfig.FailbackMaxStableWindow, "failback-max-stable-window", 0, "cap on the backed-off stable window (default: 8h)")
	flag.DurationVar(&flagConfig.FailbackCooldown, "failback-cooldown", 0, "how long a project must have run where it is before fail-back moves it (default: 6h)")
	flag.Float64Var(&flagConfig.FailbackMinFreePercent, "failback-min-free-percent", 0, "percent of a primary node's CPU and memory that must stay free after a fail-back (default: 20)")
	flag.Parse()
	return configutil.Merge[Config](cfg, flagConfig)
}
