package config

import (
	"errors"

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
	return nil
}

func (c *AgentConfig) Validate() error {
	if c.ServerURL == "" {
		return errors.New("server_url is required")
	}
	return nil
}
