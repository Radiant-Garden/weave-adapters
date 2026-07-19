package dhcpwindows

import (
	"errors"
	"fmt"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
)

// Configuration key names. The adapter owns its own keys: internal/core/config
// keeps the precedence machinery and stays adapter-agnostic, so nothing here
// requires a change in core.
const (
	KeyServer          = "dhcp.server"
	KeyPowerShellPath  = "dhcp.powershellPath"
	KeyCommandTimeout  = "dhcp.commandTimeout"
	KeyProbeTimeout    = "dhcp.probeTimeout"
	KeyDefaultPageSize = "scopes.defaultPageSize"
	KeyMaxPageSize     = "scopes.maxPageSize"
	KeyNamespaceKey    = "identity.namespaceKey"
	KeyServerName      = "identity.serverName"
)

const (
	defaultPowerShellPath = "powershell.exe"
	defaultCommandTimeout = 10 * time.Second
	defaultProbeTimeout   = 3 * time.Second
	defaultPageSize       = 50
	maxPageSizeDefault    = 500

	// minNamespaceKeyLength is a floor, not a strength check. The key's job is
	// per-installation uniqueness rather than secrecy, but a one-character key
	// is far more likely to be a placeholder someone meant to replace than a
	// deliberate choice.
	minNamespaceKeyLength = 16
)

// Config is the adapter's runtime configuration, built from the same resolved
// Values the core config is.
type Config struct {
	// Server is the -ComputerName target; empty means the local host. This is
	// a connection detail only — it is deliberately not the identity input.
	Server string
	// PowerShellPath overrides the shell binary, for pwsh or a non-default
	// location.
	PowerShellPath string
	// CommandTimeout bounds one backend invocation.
	CommandTimeout time.Duration
	// ProbeTimeout bounds the health probe specifically, and is shorter:
	// health.refresh holds its mutex across the check, so aggressive polling
	// would otherwise serialize behind a slow powershell.exe.
	ProbeTimeout time.Duration
	// DefaultPageSize and MaxPageSize bound GET /api/v1/scopes.
	DefaultPageSize int
	MaxPageSize     int
	// NamespaceKey is the HMAC key for wadaptID derivation. Required, with no
	// default, and never auto-generated — see Keys.
	NamespaceKey string
	// ServerName is the canonical server identity hashed into every wadaptID,
	// canonicalized at load. Required, with no os.Hostname() fallback.
	ServerName string
}

// Keys returns the adapter's configuration keys, to be appended to
// config.CoreKeys() by the binary's wiring.
//
// The two identity keys have no default, deliberately.
//
// identity.namespaceKey is backup-critical. It is what guarantees
// per-installation uniqueness — the server name alone does not, since two sites
// both running DHCP01 would collide — and an auto-generated key that
// regenerates on reinstall *is* a fleet-wide re-key. Losing it re-derives every
// ID at once: every scope reads as gone to weave, every recreate is rejected by
// Windows' one-scope-per-subnet rule, and the result is sync paralysis. So the
// adapter refuses to start without one rather than inventing it. Treat it like
// the token store: provisioned once, backed up, never rotated casually.
//
// identity.serverName has no os.Hostname() fallback for the same reason in a
// quieter form. os.Hostname() is *environment*, not provisioning, and an
// identity input that silently follows whatever the host happens to be called
// makes three ordinary operations fleet-wide re-key events: renaming the host,
// setting dhcp.server for the first time, and switching between the short name
// and the FQDN. The read path is stateless, so nothing persists a previous
// value to notice the change against.
//
// Both are settable from the environment, which the derived key convention
// gives for free: config.toml enforces no file mode, and an environment path is
// the standard answer for a backup-critical secret.
func Keys() config.Spec {
	return config.Spec{
		{
			Name:    KeyServer,
			Type:    config.TypeString,
			Default: "",
			Usage:   "DHCP server to query; empty means the local host",
		},
		{
			Name:    KeyPowerShellPath,
			Type:    config.TypeString,
			Default: defaultPowerShellPath,
			Usage:   "path to powershell.exe or pwsh",
		},
		{
			Name:    KeyCommandTimeout,
			Type:    config.TypeDuration,
			Default: defaultCommandTimeout,
			Usage:   "per-invocation backend timeout",
		},
		{
			Name:    KeyProbeTimeout,
			Type:    config.TypeDuration,
			Default: defaultProbeTimeout,
			Usage:   "backend timeout for the health probe specifically",
		},
		{
			Name:    KeyDefaultPageSize,
			Type:    config.TypeInt,
			Default: defaultPageSize,
			Usage:   "default page size for GET /api/v1/scopes",
		},
		{
			Name:    KeyMaxPageSize,
			Type:    config.TypeInt,
			Default: maxPageSizeDefault,
			Usage:   "maximum page size for GET /api/v1/scopes",
		},
		{
			Name:  KeyNamespaceKey,
			Type:  config.TypeString,
			Usage: "HMAC key for wadaptID derivation (required, backup-critical)",
		},
		{
			Name:  KeyServerName,
			Type:  config.TypeString,
			Usage: "canonical server identity hashed into every wadaptID (required)",
		},
	}
}

// NewConfig builds and validates the adapter configuration from resolved
// values. Like the core's Core(), it reports every problem at once rather than
// stopping at the first.
func NewConfig(v *config.Values) (Config, error) {
	cfg := Config{
		Server:          v.String(KeyServer),
		PowerShellPath:  v.String(KeyPowerShellPath),
		CommandTimeout:  v.Duration(KeyCommandTimeout),
		ProbeTimeout:    v.Duration(KeyProbeTimeout),
		DefaultPageSize: v.Int(KeyDefaultPageSize),
		MaxPageSize:     v.Int(KeyMaxPageSize),
		NamespaceKey:    v.String(KeyNamespaceKey),
		// Canonicalized once, here, so that "DHCP01", "dhcp01" and "dhcp01."
		// are one identity rather than three.
		ServerName: canonicalServerName(v.String(KeyServerName)),
	}

	return cfg, cfg.Validate()
}

// Validate reports all configuration problems at once (joined).
func (c Config) Validate() error {
	var errs []error

	if c.PowerShellPath == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", KeyPowerShellPath))
	}

	if c.CommandTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", KeyCommandTimeout, c.CommandTimeout))
	}

	if c.ProbeTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", KeyProbeTimeout, c.ProbeTimeout))
	}

	// Not merely tidiness: the probe's shorter bound is what stops a slow
	// backend serializing every health poll behind health.refresh's mutex, so a
	// probe timeout at or above the general one silently removes that
	// protection while looking configured.
	if c.ProbeTimeout > 0 && c.CommandTimeout > 0 && c.ProbeTimeout >= c.CommandTimeout {
		errs = append(errs, fmt.Errorf("%s (%s) must be shorter than %s (%s): the probe's tighter bound is what "+
			"keeps a slow backend from serializing health polls",
			KeyProbeTimeout, c.ProbeTimeout, KeyCommandTimeout, c.CommandTimeout))
	}

	errs = append(errs, c.validatePaging()...)
	errs = append(errs, c.validateIdentity()...)

	return errors.Join(errs...)
}

// validatePaging checks the page-size bounds.
func (c Config) validatePaging() []error {
	var errs []error

	if c.DefaultPageSize < 1 {
		errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", KeyDefaultPageSize, c.DefaultPageSize))
	}

	if c.MaxPageSize < 1 {
		errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", KeyMaxPageSize, c.MaxPageSize))
	}

	// A default above the maximum would be clamped away on every request, so
	// the configured default would silently never apply.
	if c.DefaultPageSize >= 1 && c.MaxPageSize >= 1 && c.DefaultPageSize > c.MaxPageSize {
		errs = append(errs, fmt.Errorf("%s (%d) must not exceed %s (%d)",
			KeyDefaultPageSize, c.DefaultPageSize, KeyMaxPageSize, c.MaxPageSize))
	}

	return errs
}

// validateIdentity checks the two required identity inputs. These are the
// errors that stop the adapter starting at all, which is the intended
// behaviour: see Keys for why neither may be defaulted.
func (c Config) validateIdentity() []error {
	var errs []error

	switch {
	case c.NamespaceKey == "":
		errs = append(errs, fmt.Errorf("%s is required and has no default: it is what makes wadaptIDs unique "+
			"per installation, and an auto-generated one would re-key the whole fleet on reinstall. "+
			"Provision it (%s), back it up like the token store, and never rotate it casually",
			KeyNamespaceKey, config.EnvName(KeyNamespaceKey)))
	case len(c.NamespaceKey) < minNamespaceKeyLength:
		errs = append(errs, fmt.Errorf("%s must be at least %d characters, got %d",
			KeyNamespaceKey, minNamespaceKeyLength, len(c.NamespaceKey)))
	}

	if c.ServerName == "" {
		errs = append(errs, fmt.Errorf("%s is required and has no os.Hostname() fallback: it is hashed into "+
			"every wadaptID, and an identity that follows whatever the host is called would re-key the fleet "+
			"on a rename. Provision it (%s)",
			KeyServerName, config.EnvName(KeyServerName)))
	}

	return errs
}
