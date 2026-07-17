// Package config loads and validates the adapter's runtime configuration.
//
// Precedence is flags > environment (WEAVE_ADAPTER_*) > TOML file > defaults.
// The common struct holds only what M1 consumes; it grows per milestone as new
// consumers land. Validate() fails fast at startup with all errors joined.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const (
	envPrefix       = "WEAVE_ADAPTER_"
	envPort         = envPrefix + "PORT"
	envDisableHTTPS = envPrefix + "DISABLE_HTTPS"
	envLogSeverity  = envPrefix + "LOG_SEVERITY"

	defaultPort        = 8444
	defaultLogSeverity = "info"

	minPort = 1
	maxPort = 65535
)

// validSeverities is the allowed set for LogSeverity.
var validSeverities = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// Config is the adapter's runtime configuration.
type Config struct {
	// Port is the TCP port the HTTP server listens on.
	Port int `koanf:"port"`
	// DisableHTTPS must be true in M1 — HTTPS/TLS is not implemented yet.
	DisableHTTPS bool `koanf:"disableHttps"`
	// LogSeverity is the log level: debug, info, warn, or error.
	LogSeverity string `koanf:"logSeverity"`
}

// Load reads configuration from flags, environment, an optional TOML file, and
// built-in defaults, then validates the result. args are the CLI arguments
// without the program name (e.g. os.Args[1:]).
func Load(args []string) (*Config, error) {
	return load(args, os.Environ)
}

// load is the testable core of Load with an injectable environment source.
func load(args []string, environ func() []string) (*Config, error) {
	configPath, overrides, err := parseFlags(args)
	if err != nil {
		return nil, err
	}

	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults(), "."), nil); err != nil {
		return nil, fmt.Errorf("loading defaults: %w", err)
	}

	if configPath != "" {
		if err := k.Load(file.Provider(configPath), toml.Parser()); err != nil {
			return nil, fmt.Errorf("loading config file %q: %w", configPath, err)
		}
	}

	envProvider := env.Provider(".", env.Opt{
		Prefix:        envPrefix,
		EnvironFunc:   environ,
		TransformFunc: transformEnv,
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("loading environment: %w", err)
	}

	if len(overrides) > 0 {
		if err := k.Load(confmap.Provider(overrides, "."), nil); err != nil {
			return nil, fmt.Errorf("loading flags: %w", err)
		}
	}

	cfg := &Config{
		Port:         k.Int("port"),
		DisableHTTPS: k.Bool("disableHttps"),
		LogSeverity:  k.String("logSeverity"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// defaults returns the built-in configuration values.
func defaults() map[string]any {
	return map[string]any{
		"port":         defaultPort,
		"disableHttps": true,
		"logSeverity":  defaultLogSeverity,
	}
}

// parseFlags parses CLI flags and returns the config file path and the set of
// keys the user explicitly overrode (only visited flags win, so unset flags
// don't clobber file/env values).
func parseFlags(args []string) (configPath string, overrides map[string]any, err error) {
	fs := flag.NewFlagSet("weave-adapter-dhcp-windows", flag.ContinueOnError)
	port := fs.Int("port", 0, "TCP listen port")
	disableHTTPS := fs.Bool("disable-https", false, "disable HTTPS (must be true in M1)")
	logSeverity := fs.String("log-severity", "", "log level: debug|info|warn|error")
	fs.StringVar(&configPath, "config", "", "path to a TOML config file")

	if err = fs.Parse(args); err != nil {
		return "", nil, err
	}

	overrides = make(map[string]any)

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			overrides["port"] = *port
		case "disable-https":
			overrides["disableHttps"] = *disableHTTPS
		case "log-severity":
			overrides["logSeverity"] = *logSeverity
		}
	})

	return configPath, overrides, nil
}

// transformEnv maps WEAVE_ADAPTER_* environment variables onto config keys.
// Unrecognized variables return an empty key and are ignored.
func transformEnv(key, value string) (string, any) {
	switch key {
	case envPort:
		return "port", value
	case envDisableHTTPS:
		return "disableHttps", value
	case envLogSeverity:
		return "logSeverity", value
	default:
		return "", nil
	}
}

// Validate reports all configuration problems at once (joined), rather than
// failing on the first one.
func (c *Config) Validate() error {
	var errs []error

	if c.Port < minPort || c.Port > maxPort {
		errs = append(errs, fmt.Errorf("port must be between %d and %d, got %d", minPort, maxPort, c.Port))
	}

	if !validSeverities[c.LogSeverity] {
		errs = append(errs, fmt.Errorf("logSeverity must be one of debug, info, warn, error, got %q", c.LogSeverity))
	}

	if !c.DisableHTTPS {
		errs = append(errs, errors.New("disableHttps must be true: HTTPS is not implemented in M1"))
	}

	return errors.Join(errs...)
}
