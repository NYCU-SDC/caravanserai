package config

import (
	"flag"
	"os"
	"time"

	configutil "github.com/NYCU-SDC/summer/pkg/config"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// AgentConfig holds the runtime configuration for cara-agent.
// It replaces the listen-address fields with the control-plane server URL that
// the agent dials out to.
type AgentConfig struct {
	Debug             bool          `yaml:"debug"               envconfig:"DEBUG"`
	ServerURL         string        `yaml:"server_url"          envconfig:"SERVER_URL"`
	OtelCollectorUrl  string        `yaml:"otel_collector_url"  envconfig:"OTEL_COLLECTOR_URL"`
	NodeName          string        `yaml:"node_name"           envconfig:"NODE_NAME"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"  envconfig:"HEARTBEAT_INTERVAL"`
	// DockerHost is the Docker daemon endpoint used by the agent to manage
	// containers.  Defaults to the Unix socket path on Linux/macOS.
	// Can be overridden with a tcp:// URL for remote Docker daemons.
	DockerHost string `yaml:"docker_host" envconfig:"DOCKER_HOST"`
	// ListenPort is the TCP port the Agent's HTTP server listens on.
	// The server exposes the port-forward WebSocket endpoint and a health
	// probe.  Defaults to "9090".
	ListenPort string `yaml:"listen_port" envconfig:"AGENT_LISTEN_PORT"`
	// ProxyListenAddr is the address the ingress reverse proxy listens on.
	// The proxy routes incoming HTTP requests to containers based on the
	// Host header using ingress rules from project specs.  Defaults to ":8081".
	ProxyListenAddr string `yaml:"proxy_listen_addr" envconfig:"PROXY_LISTEN_ADDR"`
	// AdvertiseIP is the legacy underlay address the agent advertises to the
	// control plane when overlay networking is disabled.  It is kept as a
	// backwards-compatible fallback until overlay networking is mandatory.
	AdvertiseIP string `yaml:"advertise_ip" envconfig:"AGENT_ADVERTISE_IP"`
	// HeadscaleURL is the base URL of the Headscale control plane the agent
	// joins on startup (e.g. "http://localhost:8081").  Overlay networking is
	// opt-in in 1.0: when HeadscaleURL and PreauthKeyFile are both empty the
	// agent skips the overlay join and runs on the underlay as before.  Once
	// the Headscale epic (CARA-47) is complete this is expected to become
	// mandatory.  See CARA-55.
	HeadscaleURL string `yaml:"headscale_url" envconfig:"HEADSCALE_URL"`
	// PreauthKeyFile is the path to a file containing the Headscale pre-auth
	// key used to join the overlay.  The key is read from this file and is
	// never logged.  The key itself must never be checked into the repo.
	PreauthKeyFile string `yaml:"preauth_key_file" envconfig:"HEADSCALE_PREAUTH_KEY_FILE"`
	// OverlayHostname optionally overrides the hostname the agent registers
	// with Headscale.  Defaults to NodeName (the OS hostname) when empty.
	OverlayHostname string `yaml:"overlay_hostname" envconfig:"OVERLAY_HOSTNAME"`
}

// LoadAgent reads cara-agent config from file → env → flags.
func LoadAgent() (AgentConfig, *LogBuffer) {
	logger := NewConfigLogger()

	hostname, _ := os.Hostname()

	cfg := &AgentConfig{
		Debug:             false,
		ServerURL:         "http://localhost:8080",
		NodeName:          hostname,
		HeartbeatInterval: 30 * time.Second,
		DockerHost:        "unix:///var/run/docker.sock",
		ListenPort:        "9090",
		ProxyListenAddr:   ":8081",
	}

	var err error

	cfg, err = AgentFromFile("config.yaml", cfg, logger)
	if err != nil {
		logger.Warn("Failed to load agent config from file", err, map[string]string{"path": "config.yaml"})
	}

	cfg, err = AgentFromEnv(cfg, logger)
	if err != nil {
		logger.Warn("Failed to load agent config from env", err, map[string]string{"path": ".env"})
	}

	cfg, err = AgentFromFlags(cfg)
	if err != nil {
		logger.Warn("Failed to load agent config from flags", err, nil)
	}

	return *cfg, logger
}

func AgentFromFile(filePath string, cfg *AgentConfig, logger *LogBuffer) (*AgentConfig, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return cfg, err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			logger.Warn("Failed to close config file", cerr, map[string]string{"path": filePath})
		}
	}()

	fileConfig := AgentConfig{}
	if err := yaml.NewDecoder(file).Decode(&fileConfig); err != nil {
		return cfg, err
	}

	return configutil.Merge[AgentConfig](cfg, &fileConfig)
}

func AgentFromEnv(cfg *AgentConfig, logger *LogBuffer) (*AgentConfig, error) {
	if err := godotenv.Overload(); err != nil {
		if os.IsNotExist(err) {
			logger.Warn("No .env file found", err, map[string]string{"path": ".env"})
		} else {
			return nil, err
		}
	}

	envConfig := &AgentConfig{
		Debug:            os.Getenv("DEBUG") == "true",
		ServerURL:        os.Getenv("SERVER_URL"),
		OtelCollectorUrl: os.Getenv("OTEL_COLLECTOR_URL"),
		NodeName:         os.Getenv("NODE_NAME"),
		DockerHost:       os.Getenv("DOCKER_HOST"),
		ListenPort:       os.Getenv("AGENT_LISTEN_PORT"),
		ProxyListenAddr:  os.Getenv("PROXY_LISTEN_ADDR"),
		AdvertiseIP:      os.Getenv("AGENT_ADVERTISE_IP"),
		HeadscaleURL:     os.Getenv("HEADSCALE_URL"),
		PreauthKeyFile:   os.Getenv("HEADSCALE_PREAUTH_KEY_FILE"),
		OverlayHostname:  os.Getenv("OVERLAY_HOSTNAME"),
	}

	if raw := os.Getenv("HEARTBEAT_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			envConfig.HeartbeatInterval = d
		} else {
			logger.Warn("Invalid HEARTBEAT_INTERVAL, ignoring", err, map[string]string{"value": raw})
		}
	}

	return configutil.Merge[AgentConfig](cfg, envConfig)
}

func AgentFromFlags(cfg *AgentConfig) (*AgentConfig, error) {
	flagConfig := &AgentConfig{}
	flag.BoolVar(&flagConfig.Debug, "debug", false, "enable debug mode")
	flag.StringVar(&flagConfig.ServerURL, "server-url", "", "cara-server URL")
	flag.StringVar(&flagConfig.OtelCollectorUrl, "otel_collector_url", "", "OpenTelemetry collector URL")
	flag.StringVar(&flagConfig.NodeName, "node-name", "", "node name to register with the control plane (default: hostname)")
	flag.DurationVar(&flagConfig.HeartbeatInterval, "heartbeat-interval", 0, "interval between heartbeats (default: 30s)")
	flag.StringVar(&flagConfig.DockerHost, "docker-host", "", "Docker daemon endpoint (default: unix:///var/run/docker.sock)")
	flag.StringVar(&flagConfig.ListenPort, "agent-port", "", "Agent HTTP server port (default: 9090)")
	flag.StringVar(&flagConfig.ProxyListenAddr, "proxy-listen-addr", "", "Ingress proxy listen address (default: :8081)")
	flag.StringVar(&flagConfig.AdvertiseIP, "advertise-ip", "", "legacy underlay IP address to advertise when overlay networking is disabled")
	flag.StringVar(&flagConfig.HeadscaleURL, "headscale-url", "", "Headscale control-plane URL to join on startup (enables overlay networking)")
	flag.StringVar(&flagConfig.PreauthKeyFile, "preauth-key-file", "", "path to a file containing the Headscale pre-auth key")
	flag.StringVar(&flagConfig.OverlayHostname, "overlay-hostname", "", "hostname to register with Headscale (default: node name)")
	flag.Parse()
	return configutil.Merge[AgentConfig](cfg, flagConfig)
}
