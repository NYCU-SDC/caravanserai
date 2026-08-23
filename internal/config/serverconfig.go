package config

import (
	"flag"
	"os"

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
	flag.Parse()
	return configutil.Merge[Config](cfg, flagConfig)
}
