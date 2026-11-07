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

	// TerminatingTimeout is how long a Terminating Project's Node may stay
	// NotReady before the Project is marked TerminationBlocked. Zero means the
	// controller's default (10m). It only decides when the stuck deletion is
	// reported; the record is kept either way.
	TerminatingTimeout time.Duration `yaml:"terminating_timeout" envconfig:"TERMINATING_TIMEOUT"`

	// RunningGracePeriod is how long a Running Project may stay on a NotReady
	// Node before its release is attempted. Zero means the controller's default
	// (3m).
	RunningGracePeriod time.Duration `yaml:"running_grace_period" envconfig:"RUNNING_GRACE_PERIOD"`

	// GraceExpiredRelease moves a Project with no Managed volume, or with a
	// committed backup to restore from, off a Node that stayed NotReady past the
	// grace period, with no proof it stopped. A partitioned Node keeps its copy
	// running until it reconnects (ADR-0024).
	// The assignment fence refuses the returning Agent's stale writes.
	GraceExpiredRelease bool `yaml:"grace_expired_release" envconfig:"GRACE_EXPIRED_RELEASE"`

	// BreakGlassTokenFile names a file holding the break-glass admin token
	// (ADR-0031). It authenticates as user:break-glass, and is how the first
	// tokens are issued and how an operator gets in when nothing else works.
	// Empty configures none. The token is read from a file, not a flag or an
	// environment variable, to keep it out of process listings.
	BreakGlassTokenFile string `yaml:"break_glass_token_file" envconfig:"BREAK_GLASS_TOKEN_FILE"`

	// PostgresDifferentialImage is the image reference a PostgresDifferential
	// volume's service must use. Empty means the published one,
	// ghcr.io/nycu-sdc/cara-postgres:16. Set it where that image cannot be
	// pulled, for example a development environment using a copy pushed to
	// another registry; Agents pull whatever the manifest names, so manifests
	// in that environment name the same reference.
	PostgresDifferentialImage string `yaml:"postgres_differential_image" envconfig:"POSTGRES_DIFFERENTIAL_IMAGE"`

	// MaxInFlightEvictions caps how many planned moves (drain, fail-back) run
	// at once across the cluster (CARA-119). Each move is downtime plus a
	// backup and a restore, so the default is one. Zero means the default.
	MaxInFlightEvictions int `yaml:"max_inflight_evictions" envconfig:"MAX_INFLIGHT_EVICTIONS"`

	// FailbackWindow is the daily window, "HH:MM-HH:MM" in FailbackTimezone,
	// in which Projects on the backup tier are moved back to primary
	// (CARA-121), e.g. "02:00-05:00". It may cross midnight. Empty disables
	// fail-back. Leave it empty until the anti-flap defences of CARA-122 are
	// in: without them a primary node that has only just recovered can be
	// handed Projects straight away.
	FailbackWindow string `yaml:"failback_window" envconfig:"FAILBACK_WINDOW"`
	// FailbackTimezone is the IANA time zone FailbackWindow is read in.
	// Defaults to DefaultFailbackTimezone.
	FailbackTimezone string `yaml:"failback_timezone" envconfig:"FAILBACK_TIMEZONE"`

	// configFileError is why a config file named explicitly could not be used.
	configFileError error
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

	path, explicit := resolveConfigPath(os.Args[1:])
	cfg, err = FromFile(path, cfg, logger)
	if err != nil {
		logger.Warn("Failed to load config from file", err, map[string]string{"path": path})
	}
	fileErr := explicitFileError(explicit, err)

	cfg, err = FromEnv(cfg, logger)
	if err != nil {
		logger.Warn("Failed to load config from env", err, map[string]string{"path": ".env"})
	}

	cfg, err = FromFlags(cfg)
	if err != nil {
		logger.Warn("Failed to load config from flags", err, nil)
	}

	cfg.configFileError = fileErr
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
		Debug:               os.Getenv("DEBUG") == "true",
		Host:                os.Getenv("HOST"),
		Port:                os.Getenv("PORT"),
		OtelCollectorUrl:    os.Getenv("OTEL_COLLECTOR_URL"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		HeadscaleURL:        os.Getenv("HEADSCALE_URL"),
		PreauthKeyFile:      os.Getenv("HEADSCALE_PREAUTH_KEY_FILE"),
		OverlayHostname:     os.Getenv("OVERLAY_HOSTNAME"),
		OverlayStateDir:     os.Getenv("OVERLAY_STATE_DIR"),
		HeadscaleAPIURL:     os.Getenv("HEADSCALE_API_URL"),
		HeadscaleAPIKey:     os.Getenv("HEADSCALE_API_KEY"),
		HeadscaleUser:       os.Getenv("HEADSCALE_USER"),
		BreakGlassTokenFile: os.Getenv("BREAK_GLASS_TOKEN_FILE"),
		GraceExpiredRelease: os.Getenv("GRACE_EXPIRED_RELEASE") == "true",

		PostgresDifferentialImage: os.Getenv("POSTGRES_DIFFERENTIAL_IMAGE"),
		FailbackWindow:            os.Getenv("FAILBACK_WINDOW"),
		FailbackTimezone:          os.Getenv("FAILBACK_TIMEZONE"),
	}

	if raw := os.Getenv("RUNNING_GRACE_PERIOD"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			envConfig.RunningGracePeriod = d
		} else {
			logger.Warn("Invalid RUNNING_GRACE_PERIOD, ignoring", err, map[string]string{"value": raw})
		}
	}

	if raw := os.Getenv("TERMINATING_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			envConfig.TerminatingTimeout = d
		} else {
			logger.Warn("Invalid TERMINATING_TIMEOUT, ignoring", err, map[string]string{"value": raw})
		}
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
	flag.DurationVar(&flagConfig.TerminatingTimeout, "terminating-timeout", 0, "how long a Terminating project's node may be NotReady before the deletion is reported blocked (default: 10m)")
	flag.DurationVar(&flagConfig.RunningGracePeriod, "running-grace-period", 0, "how long a Running Project may stay on a NotReady node before its release is attempted (default: 3m)")
	flag.BoolVar(&flagConfig.GraceExpiredRelease, "grace-expired-release", false, "move Projects off a node that stayed NotReady past the grace period; one with a Managed volume needs a committed backup")
	flag.StringVar(&flagConfig.BreakGlassTokenFile, "break-glass-token-file", "", "file holding the break-glass admin token")
	flag.String(configFlag, "", configFlagUsage)
	flag.IntVar(&flagConfig.MaxInFlightEvictions, "max-inflight-evictions", 0, "planned moves (drain, fail-back) allowed at once across the cluster (default: 1)")
	flag.StringVar(&flagConfig.FailbackWindow, "failback-window", "", "daily window HH:MM-HH:MM in which projects are moved back to the primary tier (default: disabled)")
	flag.StringVar(&flagConfig.FailbackTimezone, "failback-timezone", "", "IANA time zone the fail-back window is read in (default: Asia/Taipei)")
	flag.Parse()
	return configutil.Merge[Config](cfg, flagConfig)
}
