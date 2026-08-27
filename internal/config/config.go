package config

import (
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

var ErrOtelCollectorURLRequired = errors.New("otel_collector_url is required in production mode")

// LogBuffer defers config-load warnings until a real zap.Logger is available.
type LogBuffer struct {
	buffer []logEntry
}

type logEntry struct {
	msg  string
	err  error
	meta map[string]string
}

func NewConfigLogger() *LogBuffer {
	return &LogBuffer{}
}

func (cl *LogBuffer) Warn(msg string, err error, meta map[string]string) {
	cl.buffer = append(cl.buffer, logEntry{msg: msg, err: err, meta: meta})
}

func (cl *LogBuffer) FlushToZap(logger *zap.Logger) {
	for _, e := range cl.buffer {
		var fields []zap.Field
		if e.err != nil {
			fields = append(fields, zap.Error(e.err))
		}
		for k, v := range e.meta {
			fields = append(fields, zap.String(k, v))
		}
		logger.Warn(e.msg, fields...)
	}
	cl.buffer = nil
}

func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("database_url is required")
	}
	// Overlay networking is opt-in: either both HeadscaleURL and
	// PreauthKeyFile are set (overlay enabled) or neither is (overlay
	// skipped).  A half-configured overlay is almost always a mistake, so
	// reject it rather than silently skipping the join.
	if (c.HeadscaleURL == "") != (c.PreauthKeyFile == "") {
		return errors.New("headscale_url and preauth_key_file must be set together (or both left empty to disable overlay networking)")
	}
	return nil
}

func (c *AgentConfig) Validate() error {
	if c.ServerURL == "" {
		return errors.New("server_url is required")
	}
	// Overlay networking is opt-in: either both HeadscaleURL and
	// PreauthKeyFile are set (overlay enabled) or neither is (overlay
	// skipped).  A half-configured overlay is almost always a mistake, so
	// reject it rather than silently skipping the join.
	if (c.HeadscaleURL == "") != (c.PreauthKeyFile == "") {
		return errors.New("headscale_url and preauth_key_file must be set together (or both left empty to disable overlay networking)")
	}
	if c.HeadscaleURL == "" && c.AdvertiseIP == "" {
		return errors.New("advertise_ip is required when overlay networking is disabled (set AGENT_ADVERTISE_IP or --advertise-ip)")
	}
	if c.InsecureSecretFile != "" {
		return fmt.Errorf(
			"%s contains s3.secret_key but is readable beyond its owner; run: chmod 0600 %s",
			c.InsecureSecretFile, c.InsecureSecretFile,
		)
	}
	return c.S3.Validate()
}

// Validate checks that a configured object store is fully specified. An empty
// endpoint means backups are disabled, which is valid; anything else must
// carry a bucket and credentials so the agent fails at startup rather than at
// the first backup a week later.
func (s *S3Config) Validate() error {
	if !s.Enabled() {
		return nil
	}
	scheme, _, found := strings.Cut(s.Endpoint, "://")
	if !found || (scheme != "http" && scheme != "https") {
		return fmt.Errorf("s3.endpoint %q must start with http:// or https://", s.Endpoint)
	}
	if s.Bucket == "" {
		return errors.New("s3.bucket is required when s3.endpoint is set")
	}
	if s.AccessKey == "" {
		return errors.New("s3.access_key is required when s3.endpoint is set")
	}
	if s.SecretKey == "" {
		return errors.New("s3.secret_key is required when s3.endpoint is set")
	}
	return nil
}
