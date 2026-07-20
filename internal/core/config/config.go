// Package config loads and validates the adapter's runtime configuration.
//
// Precedence is flags > environment (WEAVE_ADAPTER_*) > TOML file > defaults.
// The package is adapter-agnostic: it owns the precedence machinery and the
// key-registration primitives, but not the key set. CoreKeys declares the
// adapter-agnostic keys; each adapter registers its own and builds its own
// struct from the same Values, so one precedence pass serves both.
//
// The naming convention is derived rather than declared: a dotted camelCase key
// maps to one flag and one environment variable, mechanically.
//
//	dhcp.commandTimeout  ->  -dhcp-command-timeout  ->  WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT
//
// Validate() fails fast at startup with all errors joined.
package config

import (
	"errors"
	"fmt"

	"github.com/go-playground/validator/v10"
)

const (
	envPrefix = "WEAVE_ADAPTER_"

	defaultPort        = 8444
	defaultLogSeverity = "info"

	// defaultMaxRequestBodyBytes bounds a request body at 1 MiB. The bodies this
	// adapter reads are single resources — a scope create is a few hundred bytes
	// — so this is three orders of magnitude of headroom rather than a limit an
	// honest client will meet. It exists to stop an unbounded read, not to size
	// the payload.
	defaultMaxRequestBodyBytes = 1 << 20

	minPort = 1
	maxPort = 65535
)

// Core configuration key names. Exported so a binary's wiring and its tests
// refer to a key by constant rather than by a string literal that a rename
// would leave behind.
const (
	KeyPort         = "port"
	KeyDisableHTTPS = "disableHttps"
	KeyLogSeverity  = "logSeverity"
	//nolint:gosec // G101: a config key name, not a credential — it names the path to the store.
	KeyAuthTokensFile = "authTokensFile"
	KeyDisableAuth    = "disableAuth"

	KeyMaxRequestBodyBytes = "maxRequestBodyBytes"
)

// DefaultAuthTokensFile is the token store both the server and the token CLI
// default to. Exported and shared, because the two halves drifting apart means
// the CLI writes tokens where the server never looks -- a failure that surfaces
// as "no tokens configured" for a file the operator can see is full.
//
// It is deliberately a separate file from config.toml: this one is
// machine-owned and rewritten wholesale, and go-toml/v2 does not preserve
// comments on round-trip, so rewriting the hand-edited config would silently eat
// the operator's comments.
const DefaultAuthTokensFile = "tokens.toml"

// validate is the shared struct validator (safe for concurrent use, caches
// struct metadata).
var validate = validator.New(validator.WithRequiredStructEnabled())

// CoreKeys returns the adapter-agnostic keys every binary registers. An adapter
// appends its own to this spec; it never edits it.
func CoreKeys() Spec {
	return Spec{
		{
			Name:    KeyPort,
			Type:    TypeInt,
			Default: defaultPort,
			Usage:   "TCP listen port",
		},
		{
			Name:    KeyDisableHTTPS,
			Type:    TypeBool,
			Default: true,
			Usage:   "disable HTTPS (must be true in M1)",
		},
		{
			Name:    KeyLogSeverity,
			Type:    TypeString,
			Default: defaultLogSeverity,
			Usage:   "log level: debug|info|warn|error",
		},
		{
			Name:    KeyAuthTokensFile,
			Type:    TypeString,
			Default: DefaultAuthTokensFile,
			Usage:   "path to the bearer token store",
		},
		{
			Name:    KeyDisableAuth,
			Type:    TypeBool,
			Default: false,
			Usage:   "disable bearer authentication (development only)",
		},
		{
			Name:    KeyMaxRequestBodyBytes,
			Type:    TypeInt,
			Default: defaultMaxRequestBodyBytes,
			Usage:   "maximum accepted request body size in bytes",
		},
	}
}

// Config is the adapter-agnostic runtime configuration. Field rules are
// expressed as validator struct tags and checked by Validate.
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
	// MaxRequestBodyBytes bounds every request body the adapter reads. A body
	// that exceeds it is rejected with 413 before the decoder sees it.
	//
	// There is no "unlimited" value on purpose: 0 and negative are rejected
	// rather than read as off. An operator who wants a bigger body raises the
	// number, and the one who fat-fingers it to 0 gets a startup error instead
	// of a server that will read a body until it runs out of memory.
	MaxRequestBodyBytes int `koanf:"maxRequestBodyBytes" validate:"min=1"`
}

// Core builds and validates the core configuration from resolved values. It
// panics if the core keys were not registered, which is a wiring mistake: every
// binary composes its spec from CoreKeys.
func Core(v *Values) (*Config, error) {
	cfg := &Config{
		Port:           v.Int(KeyPort),
		DisableHTTPS:   v.Bool(KeyDisableHTTPS),
		LogSeverity:    v.String(KeyLogSeverity),
		AuthTokensFile: v.String(KeyAuthTokensFile),
		DisableAuth:    v.Bool(KeyDisableAuth),

		MaxRequestBodyBytes: v.Int(KeyMaxRequestBodyBytes),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
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
	case "MaxRequestBodyBytes":
		// Named rather than left to the fallback because the fallback prints the
		// Go field name, and an operator reading it has a config key in front of
		// them. It also says there is no unlimited setting, which is the mistake
		// a 0 most likely was.
		return fmt.Errorf("maxRequestBodyBytes must be at least 1 byte (there is no unlimited setting), got %v", fe.Value())
	default:
		// Defensive fallback for a tagged field without a bespoke message. Only
		// Port and LogSeverity carry validate tags today, so this is unreachable
		// until a third tagged field is added; it then yields a generic message
		// rather than nothing.
		return fmt.Errorf("%s failed %q validation", fe.Field(), fe.Tag())
	}
}
