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
	flag.Parse()
	return configutil.Merge[Config](cfg, flagConfig)
}
