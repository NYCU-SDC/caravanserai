package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	configFlag      = "config"
	configFlagUsage = "config file (env: CARA_CONFIG; default: ./config.yaml, then /etc/cara/config.yaml)"

	localConfigPath  = "config.yaml"
	systemConfigPath = "/etc/cara/config.yaml"
)

// resolveConfigPath picks the config file before flags are parsed, because the
// file is the lowest layer and flags have to override it. explicit reports that
// the caller named the file, so a missing one is an error rather than a default
// that happens to be absent.
func resolveConfigPath(args []string) (path string, explicit bool) {
	for i, arg := range args {
		name := strings.TrimLeft(arg, "-")
		if name == arg {
			continue
		}
		if value, ok := strings.CutPrefix(name, configFlag+"="); ok {
			return value, true
		}
		if name == configFlag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	if env := os.Getenv("CARA_CONFIG"); env != "" {
		return env, true
	}
	if _, err := os.Stat(localConfigPath); err == nil {
		return localConfigPath, false
	}
	if _, err := os.Stat(systemConfigPath); err == nil {
		return systemConfigPath, false
	}
	return localConfigPath, false
}

// explicitFileError keeps a load failure only when the caller named the file.
func explicitFileError(explicit bool, err error) error {
	if !explicit || err == nil {
		return nil
	}
	return fmt.Errorf("config file: %w", err)
}
