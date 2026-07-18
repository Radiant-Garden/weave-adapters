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

	"github.com/go-playground/validator/v10"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const (
	envPrefix         = "WEAVE_ADAPTER_"
	envPort           = envPrefix + "PORT"
	envDisableHTTPS   = envPrefix + "DISABLE_HTTPS"
	envLogSeverity    = envPrefix + "LOG_SEVERITY"
	envAuthTokensFile = envPrefix + "AUTH_TOKENS_FILE"
	envDisableAuth    = envPrefix + "DISABLE_AUTH"

	defaultPort           = 8444
	defaultLogSeverity    = "info"
	defaultAuthTokensFile = "tokens.toml"

	minPort = 1
	maxPort = 65535
)

// validate is the shared struct validator (safe for concurrent use, caches
// struct metadata).
var validate = validator.New(validator.WithRequiredStructEnabled())

// Config is the adapter's runtime configuration. Field rules are expressed as
// validator struct tags and checked by Validate.
type Config struct {
	// Port is the TCP port the HTTP server listens on.
	Port int `koanf:"port" validate:"min=1,max=65535"`
	// DisableHTTPS must be true in M1 — HTTPS/TLS is not implemented yet.
	DisableHTTPS bool `koanf:"disableHttps"`
	// LogSeverity is the log level: debug, info, warn, or error.
	LogSeverity string `koanf:"logSeverity" validate:"oneof=debug info warn error"`
	// AuthTokensFile is the path to the token store the `token` subcommand
	// manages. It is read once at startup; changes need a restart.
	AuthTokensFile string `koanf:"authTokensFile"`
	// DisableAuth turns off bearer authentication. Development only — it leaves
	// every route open to anyone who can reach the port.
	DisableAuth bool `koanf:"disableAuth"`
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
		Port:           k.Int("port"),
		DisableHTTPS:   k.Bool("disableHttps"),
		LogSeverity:    k.String("logSeverity"),
		AuthTokensFile: k.String("authTokensFile"),
		DisableAuth:    k.Bool("disableAuth"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// defaults returns the built-in configuration values.
func defaults() map[string]any {
	return map[string]any{
		"port":           defaultPort,
		"disableHttps":   true,
		"logSeverity":    defaultLogSeverity,
		"authTokensFile": defaultAuthTokensFile,
		"disableAuth":    false,
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
	authTokensFile := fs.String("auth-tokens-file", "", "path to the bearer token store")
	disableAuth := fs.Bool("disable-auth", false, "disable bearer authentication (development only)")
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
		case "auth-tokens-file":
			overrides["authTokensFile"] = *authTokensFile
		case "disable-auth":
			overrides["disableAuth"] = *disableAuth
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
	case envAuthTokensFile:
		return "authTokensFile", value
	case envDisableAuth:
		return "disableAuth", value
	default:
		return "", nil
	}
}

// Validate reports all configuration problems at once (joined), rather than
// failing on the first one. Field rules run through the validator; the
// disableHttps rule is M1-specific and checked separately.
func (c *Config) Validate() error {
	var errs []error

	var verrs validator.ValidationErrors
	if err := validate.Struct(c); err != nil && errors.As(err, &verrs) {
		for _, fe := range verrs {
			errs = append(errs, fieldError(fe))
		}
	} else if err != nil {
		// Defensive: validate.Struct only returns a non-ValidationErrors error
		// for a programmer mistake (a nil or non-struct target). Validate always
		// passes a *Config, so this branch is unreachable today; it is kept so a
		// future misuse surfaces as an error instead of being silently dropped.
		errs = append(errs, err)
	}

	if !c.DisableHTTPS {
		errs = append(errs, errors.New("disableHttps must be true: HTTPS is not implemented in M1"))
	}

	// An empty path with auth on would send the token loader looking at "", so
	// catch it here where the message can name the key.
	if !c.DisableAuth && c.AuthTokensFile == "" {
		errs = append(errs, errors.New("authTokensFile must be set unless disableAuth is true"))
	}

	return errors.Join(errs...)
}

// fieldError renders a validator field error as an operator-friendly message.
func fieldError(fe validator.FieldError) error {
	switch fe.Field() {
	case "Port":
		return fmt.Errorf("port must be between %d and %d, got %v", minPort, maxPort, fe.Value())
	case "LogSeverity":
		return fmt.Errorf("logSeverity must be one of debug, info, warn, error, got %q", fe.Value())
	default:
		// Defensive fallback for a tagged field without a bespoke message. Only
		// Port and LogSeverity carry validate tags today, so this is unreachable
		// until a third tagged field is added; it then yields a generic message
		// rather than nothing.
		return fmt.Errorf("%s failed %q validation", fe.Field(), fe.Tag())
	}
}
