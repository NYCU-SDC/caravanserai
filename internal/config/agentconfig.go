package config

import (
	"flag"
	"os"
	"time"

	configutil "github.com/NYCU-SDC/summer/pkg/config"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// secretFileMaxMode is the most permissive mode allowed for a config file that
// carries S3 credentials: owner read/write only.
const secretFileMaxMode os.FileMode = 0o600

// defaultMinFreeBytes is the free-space floor applied to Managed volume
// backups and restores when the config does not set one.
const defaultMinFreeBytes uint64 = 2 << 30 // 2 GiB

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
	// AdvertiseIP is the IP address the agent advertises to the control plane
	// in heartbeats.  Used by caractl port-forward to reach the agent.
	// This is a temporary workaround until Headscale overlay networking is
	// implemented (see CARA-30).
	AdvertiseIP string `yaml:"advertise_ip" envconfig:"AGENT_ADVERTISE_IP"`
	// DataRoot is the directory the agent owns for Managed volume data.
	// Volumes live at {DataRoot}/volumes/{namespace}/{project}/{volume}/data
	// and are bind-mounted into containers.  Defaults to "/var/lib/cara".
	// It must be on a filesystem with room for both volume data and backup
	// staging.
	DataRoot string `yaml:"data_root" envconfig:"AGENT_DATA_ROOT"`
	// MinFreeBytes is the free-space floor on the DataRoot filesystem, below
	// which a backup or a restore is refused before it touches anything.
	// Defaults to 2 GiB; set to 0 to disable the check.
	//
	// A restore transiently needs roughly 2-3x the volume set's size — the
	// compressed archives, the extracted staging copy, and the displaced
	// previous contents all exist at once — and a backup needs room for the
	// archives it is building. A flat floor cannot express that, so it is a
	// backstop against starting work on an already-full disk rather than a
	// guarantee the work will fit.
	MinFreeBytes uint64 `yaml:"min_free_bytes" envconfig:"AGENT_MIN_FREE_BYTES"`
	// S3 configures the object store used for Managed volume backups.
	// Leaving Endpoint empty disables backups; see S3Config.
	S3 S3Config `yaml:"s3"`
	// InsecureSecretFile names a config file that carries S3 credentials with
	// permissions wider than 0600.  It is set during loading and turned into a
	// startup failure by Validate; it is never read from YAML or env.
	InsecureSecretFile string `yaml:"-"`
}

// S3Config holds the object-store settings used for Managed volume backups
// (CARA-59).  Credentials live in the agent config file for 1.0 and will move
// to Infisical later, so the file must not be readable beyond its owner —
// Validate rejects a config file carrying SecretKey with permissions wider
// than 0600.
//
// TLS is derived from the Endpoint scheme (https:// enables it, http://
// disables it) rather than a separate flag, so a single value describes the
// connection.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}

// Enabled reports whether object-store backups are configured.  Managed
// volumes are provisioned and mounted regardless; only the upload side is
// gated on this.
func (s S3Config) Enabled() bool {
	return s.Endpoint != ""
}

// mergeS3 overlays the non-empty fields of override onto base.
//
// configutil.Merge only walks top-level fields and replaces a struct field
// wholesale as soon as any part of it is non-zero, so letting it handle S3
// would let a lone S3_ENDPOINT env var silently wipe the bucket and
// credentials loaded from file.  S3 is therefore merged field by field.
func mergeS3(base, override S3Config) S3Config {
	if override.Endpoint != "" {
		base.Endpoint = override.Endpoint
	}
	if override.Bucket != "" {
		base.Bucket = override.Bucket
	}
	if override.Region != "" {
		base.Region = override.Region
	}
	if override.AccessKey != "" {
		base.AccessKey = override.AccessKey
	}
	if override.SecretKey != "" {
		base.SecretKey = override.SecretKey
	}
	return base
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
		DataRoot:          "/var/lib/cara",
		MinFreeBytes:      defaultMinFreeBytes,
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

	// A config file carrying S3 credentials must not be readable by group or
	// world.  Record the violation rather than failing here: LoadAgent only
	// warns on file errors, so an error would silently drop the S3 settings
	// and leave backups disabled without anyone noticing.
	if fileConfig.S3.SecretKey != "" {
		info, statErr := file.Stat()
		if statErr != nil {
			logger.Warn("Failed to stat config file for permission check", statErr, map[string]string{"path": filePath})
		} else if info.Mode().Perm()&^secretFileMaxMode != 0 {
			fileConfig.InsecureSecretFile = filePath
		}
	}

	return mergeAgentConfig(cfg, &fileConfig)
}

// mergeAgentConfig merges override onto cfg via configutil.Merge, then
// replaces the wholesale-merged S3 field with a field-by-field merge (see
// mergeS3) so a partially-set override never wipes previously loaded S3
// settings. Shared by AgentFromFile and AgentFromEnv, the two layers that can
// each carry a partial S3Config.
func mergeAgentConfig(cfg, override *AgentConfig) (*AgentConfig, error) {
	s3 := mergeS3(cfg.S3, override.S3)
	merged, err := configutil.Merge[AgentConfig](cfg, override)
	if err != nil {
		return cfg, err
	}
	merged.S3 = s3
	return merged, nil
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
		S3: S3Config{
			Endpoint:  os.Getenv("S3_ENDPOINT"),
			Bucket:    os.Getenv("S3_BUCKET"),
			Region:    os.Getenv("S3_REGION"),
			AccessKey: os.Getenv("S3_ACCESS_KEY"),
			SecretKey: os.Getenv("S3_SECRET_KEY"),
		},
	}

	if raw := os.Getenv("HEARTBEAT_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			envConfig.HeartbeatInterval = d
		} else {
			logger.Warn("Invalid HEARTBEAT_INTERVAL, ignoring", err, map[string]string{"value": raw})
		}
	}

	return mergeAgentConfig(cfg, envConfig)
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
	flag.StringVar(&flagConfig.AdvertiseIP, "advertise-ip", "", "IP address to advertise to the control plane (required)")
	flag.Parse()
	return configutil.Merge[AgentConfig](cfg, flagConfig)
}
