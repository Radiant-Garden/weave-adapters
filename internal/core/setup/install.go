package setup

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// InstallOptions is everything Install needs that core cannot know.
type InstallOptions struct {
	// Definition is the registration template. Install fills in BinPath and
	// Args; the rest — the service name, what services.msc shows, the drain
	// budget, the recovery schedule — comes from the binary, which is the one
	// place that knows which adapter this is.
	Definition winsvc.Definition

	// BinPath is the executable to register.
	//
	// A value rather than os.Executable(), because the caller is not always
	// registering the file it is running from: setup may copy the binary into
	// %ProgramFiles% first — `install` refuses a binary directory an
	// unprivileged account can write, which is every Downloads folder — and
	// must then register the destination.
	BinPath string

	// ConfigPath is the config file the service will be started with.
	ConfigPath string

	// Spec is every registered key, core's plus the adapter's, so that the
	// resolution here is the one the server will perform.
	Spec config.Spec

	// Validate is the adapter's own configuration check, in the shape of
	// dhcpwindows.NewConfig.
	//
	// A value because it is the one adapter-bound step: core must never import
	// internal/adapters, so the check either arrives like this or vanishes
	// from the install path — and a service registered for a configuration the
	// adapter would reject fails every start, three SCM retries deep, where
	// only Event Viewer records it.
	Validate func(*config.Values) error
}

// InstallResult reports what Install registered and locked down, so the caller
// can render it. Install prints nothing itself.
type InstallResult struct {
	// BinPath and ConfigPath are the absolute paths as registered.
	BinPath    string
	ConfigPath string

	// Definition is what was registered, BinPath and Args filled in.
	Definition winsvc.Definition

	// Secured reports each lockdown target and whether it was applied. A
	// target that does not exist yet is reported, not silently dropped: the
	// token store legitimately does not exist before the first mint, and an
	// operator told it was secured would believe a later `token gen` landed in
	// a protected file.
	Secured []winsvc.SecureResult
}

// Install registers the service and locks down the files its configuration
// names.
//
// The order is the point, and each step is a refusal the operator gets now
// rather than a failure they read in Event Viewer later: resolve the
// configuration the way the service will, validate it as fully as the server
// would, check the directory the service will execute from, and only then
// register. Securing comes last because it is the only step whose failure
// leaves something behind.
func Install(opts InstallOptions, deps Deps) (InstallResult, error) {
	if err := opts.check(); err != nil {
		return InstallResult{}, err
	}

	// Resolved the way the server will, and WITHOUT the environment. The
	// caller's shell is an elevated operator's; the service gets the machine
	// environment under the SCM. A value exported here would make every check
	// below pass and then be absent at every boot, which is a confident yes
	// for a service that cannot start — worse than not checking.
	//
	// Checking the whole resolved configuration rather than the arguments this
	// call was handed is what catches a relative authTokensFile set inside the
	// TOML, which is where it is most likely to be.
	values, err := config.LoadWithoutEnvironment(opts.Spec, []string{"--config", opts.ConfigPath})
	if err != nil {
		return InstallResult{}, fmt.Errorf("reading %q: %w", opts.ConfigPath, err)
	}

	// Validated as fully as the server will, not merely parsed. The commonest
	// way for a service to fail every start is a configuration that would have
	// been rejected at the first console run — a missing identity.namespaceKey
	// above all — and the difference between catching it here and catching it
	// on the host is an error the operator reads now versus one they read in
	// Event Viewer after the SCM has retried three times.
	_, coreErr := config.Core(values)

	if err := errors.Join(config.CheckServicePaths(values), coreErr, opts.Validate(values)); err != nil {
		return InstallResult{}, fmt.Errorf("this configuration would not start as a service:\n%w", err)
	}

	binPath, err := filepath.Abs(opts.BinPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("resolving %q: %w", opts.BinPath, err)
	}

	absConfig, err := filepath.Abs(opts.ConfigPath)
	if err != nil {
		return InstallResult{}, fmt.Errorf("resolving %q: %w", opts.ConfigPath, err)
	}

	// The directory the SERVICE will execute from, checked before anything is
	// registered. A LocalSystem process launched from a folder a non-admin can
	// write is a full escalation: replace the exe and Windows runs it as
	// SYSTEM at the next start.
	//
	// Checked, never repaired. The binary may live in Program Files, whose ACL
	// is Windows' to own; rewriting it would be worse than reporting it.
	if err := deps.CheckDir(filepath.Dir(binPath)); err != nil {
		return InstallResult{}, fmt.Errorf(
			"the service would run as LocalSystem from a directory others can write to:\n%w", err)
	}

	m, err := deps.NewManager()
	if err != nil {
		return InstallResult{}, err
	}

	defer func() { _ = m.Close() }()

	// Unquoted, deliberately: the SCM registration escapes the path and every
	// argument itself, and quoting here would produce a doubly-escaped
	// ImagePath and a service that cannot start.
	def := opts.Definition
	def.BinPath = binPath
	def.Args = []string{"--config", absConfig}

	if err := m.Install(def); err != nil {
		return InstallResult{}, err
	}

	// After registration, because a failure here leaves a registered service
	// whose paths are still wide — and the error says exactly that, so an
	// operator knows the service exists and what is still owed.
	secured, err := SecurePaths(deps, absConfig, values)
	if err != nil {
		return InstallResult{}, fmt.Errorf("%s is registered, but securing its files failed: %w", def.Name, err)
	}

	return InstallResult{
		BinPath:    binPath,
		ConfigPath: absConfig,
		Definition: def,
		Secured:    secured,
	}, nil
}

// check reports a caller that did not supply what Install cannot do without.
//
// Errors rather than panics, even though every one of these is a wiring
// mistake: Install already returns an error, and the alternative is a
// privileged command that crashes instead of saying what is missing.
//
// Validate is the one worth spelling out. A nil there would not fail — it
// would silently skip the adapter's own configuration check and register a
// service that cannot start, which is exactly the failure this function exists
// to prevent.
func (o InstallOptions) check() error {
	var errs []error

	if o.ConfigPath == "" {
		errs = append(errs, errors.New("setup: InstallOptions.ConfigPath is required: "+
			"identity.namespaceKey cannot be passed as a flag, and every environment channel under the "+
			"SCM is readable from the registry, so the config file is the only way the service can be given it"))
	}

	if o.BinPath == "" {
		errs = append(errs, errors.New("setup: InstallOptions.BinPath is required: "+
			"it is what the SCM will launch, and an empty one resolves to the working directory"))
	}

	if o.Validate == nil {
		errs = append(errs, errors.New("setup: InstallOptions.Validate is required: "+
			"without it the adapter's own configuration check is skipped and a service that cannot "+
			"start is registered anyway"))
	}

	return errors.Join(errs...)
}
