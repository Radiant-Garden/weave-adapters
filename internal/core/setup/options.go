package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// defaultVerifyDeadline bounds the wait for the service to come up healthy.
//
// Thirty seconds because the local DHCP Server service was measured reaching
// Running about four seconds after ours does on Windows Server 2022, and a
// cold probe against it costs roughly two more. The budget is for the service
// starting, not for a backend that is genuinely down: an unhealthy component
// at the deadline is an outcome with its own exit code, not a failure.
const defaultVerifyDeadline = 30 * time.Second

// Response is what one HTTP probe came back with.
//
// A value rather than an *http.Response so the steps that read it never have
// to remember to close a body, and so the verification logic is testable
// without a server.
type Response struct {
	Status int
	Body   []byte
}

// GetFunc performs one GET, sending bearer as the Authorization token when it
// is not empty.
type GetFunc func(ctx context.Context, url, bearer string) (Response, error)

// Layout is where setup puts the files it provisions.
//
// One directory for all three, because the lockdown happens to the DIRECTORY
// before anything is written into it. On a default C:\ProgramData ACL a new
// file created by an elevated process inherits read for local Users, so a
// config written first and secured afterwards is world-readable in between —
// with identity.namespaceKey in it.
type Layout struct {
	// Dir is created and locked down before any file is written into it.
	Dir string

	// ConfigPath, TokenStorePath and LogPath are the files inside Dir. They
	// are spelled out rather than derived from Dir so a caller can place one
	// elsewhere without this package inventing a naming convention.
	ConfigPath     string
	TokenStorePath string
	LogPath        string

	// BinDir is where the binary is copied. It is a separate directory
	// because `install` refuses a binary whose directory an unprivileged
	// account can write, which is every Downloads folder — the destination
	// belongs under %ProgramFiles%.
	BinDir string
}

// check reports a layout that could not be used.
func (l Layout) check() error {
	var errs []error

	for name, path := range map[string]string{
		"Dir":            l.Dir,
		"ConfigPath":     l.ConfigPath,
		"TokenStorePath": l.TokenStorePath,
		"LogPath":        l.LogPath,
	} {
		if path == "" {
			errs = append(errs, fmt.Errorf("setup: Layout.%s is required", name))

			continue
		}

		// Absolute, always. Under the SCM the working directory is
		// C:\Windows\System32, so a relative path here provisions one place
		// and is read from another.
		//
		// Either rule satisfies it. Windows' is the one that finally decides —
		// it is what CheckServicePaths applies to these same values once they
		// are in the config file — and on Windows the host's rule is narrower
		// than it looks, since filepath.IsAbs rejects a drive-less "\x" there.
		// So nothing is loosened where it matters, and accepting the host's
		// rule keeps this package drivable from a developer machine, which is
		// the entire reason its platform operations are injected.
		if !config.IsAbsoluteServicePath(path) && !filepath.IsAbs(path) {
			errs = append(errs, fmt.Errorf(
				"setup: Layout.%s must be absolute, got %q: a service resolves it from %s",
				name, path, config.ServiceWorkingDirectory))
		}
	}

	return errors.Join(errs...)
}

// Options is everything a provisioning run needs that core cannot know.
//
// It is spellable without a DHCP word, which is the test of whether this
// package belongs in core: a config.Spec, a service definition template, a
// layout, the values to provision, and the two adapter-bound functions. The
// binary supplies all of it, the way it supplies routes to httpserver.
type Options struct {
	// Spec is every registered key: core's plus the adapter's.
	Spec config.Spec

	// Definition is the service registration template. BinPath and Args are
	// filled in by the install step.
	Definition winsvc.Definition

	// Validate is the adapter's own configuration check. Required — see
	// InstallOptions.Validate for why a nil one is worse than none.
	Validate func(*config.Values) error

	// HealthComponent names the health component that must be healthy for the
	// run to be a complete success. Core polls health and reports every
	// component; which one MATTERS is the binary's to say.
	HealthComponent string

	// Equivalent reports whether a provisioned value MEANS the same as the one
	// an existing configuration already resolves to.
	//
	// Adapter-bound, like Validate, because sameness is not always string
	// equality and only the adapter knows where it is not. identity.serverName
	// is canonicalized before use — lower-cased and stripped of a trailing dot,
	// so that "WIN-01.example.test." and "win-01.example.test" are one identity
	// — and a raw comparison would refuse a re-run that spelled it differently
	// while the adapter considers the two identical. That refusal is a false
	// one: it blocks a run that would have changed nothing.
	//
	// A nil Equivalent falls back to ==, which is right for every key whose
	// value is used as written.
	Equivalent func(key string, provisioned, resolved any) bool

	// ProtectedPath is a route the bearer middleware guards, used to prove
	// that the service is reading the token store this run wrote.
	//
	// Adapter-bound, like Validate: core mounts only health and the spec
	// document, and health is skipped by the auth middleware — so there is no
	// route core could name that would prove anything. An empty value skips
	// the check with a reason rather than passing it silently.
	ProtectedPath string

	// BinPath is the executable this run was started from.
	BinPath string

	// Layout is where setup provisions when it provisions its own.
	Layout Layout

	// ConfigPath overrides Layout.ConfigPath. It exists for a host whose
	// config an operator already wrote and placed somewhere of their own.
	ConfigPath string

	// Provisioned is what to write into a config file that does not exist
	// yet. It is never used to rewrite one that does: an existing config is
	// validated, and a provisioned value that disagrees with it is refused
	// rather than applied.
	Provisioned []config.Provisioned

	// TokenLabel is the label for the first token. Required, and not
	// defaulted: Store.Add refuses a duplicate, so a re-run against a store
	// whose same-label token has expired fails at the mint rather than
	// quietly minting a second one. Making the operator name it is what makes
	// that failure legible.
	TokenLabel string

	// TokenExpiresInDays bounds the first token. Zero means it never expires.
	TokenExpiresInDays int

	// DryRun runs every Check and applies nothing.
	DryRun bool

	// NoCopy registers the binary where it already is instead of copying it
	// into Layout.BinDir.
	NoCopy bool

	// Reinstall clears a Blocked registration that differs from the desired
	// one. Restart clears a Blocked step that needs the service stopped or
	// restarted. Neither is implied by anything.
	Reinstall bool
	Restart   bool

	// VerifyDeadline bounds the wait for the service to answer health. Zero
	// takes defaultVerifyDeadline.
	VerifyDeadline time.Duration

	// Now is the clock, injected so expiry handling is testable.
	Now func() time.Time
}

// check reports a caller that did not supply what Run cannot do without.
func (o Options) check() error {
	var errs []error

	if len(o.Spec) == 0 {
		errs = append(errs, errors.New("setup: Options.Spec is required: "+
			"without the key set there is nothing to resolve or to render"))
	}

	if o.Validate == nil {
		errs = append(errs, errors.New("setup: Options.Validate is required: "+
			"without it the adapter's own configuration check is skipped and a service that cannot "+
			"start is provisioned anyway"))
	}

	if o.BinPath == "" {
		errs = append(errs, errors.New("setup: Options.BinPath is required: "+
			"it is what will be copied and registered"))
	}

	if o.TokenLabel == "" {
		errs = append(errs, errors.New("setup: Options.TokenLabel is required: "+
			"the token store refuses a duplicate label, so a re-run against an expired token of the "+
			"same label fails at the mint — naming the label is what makes that legible"))
	}

	if o.HealthComponent == "" {
		errs = append(errs, errors.New("setup: Options.HealthComponent is required: "+
			"core reports every component, and which one must be healthy is the binary's to name"))
	}

	if err := o.Layout.check(); err != nil {
		errs = append(errs, err)
	}

	// Only the fields a TEMPLATE owns. BinPath and Args are filled in by the
	// install step from what the binary step chose, so Definition.Validate
	// here would refuse every correct caller for a field it is not their job
	// to set — and the full check still runs inside Manager.Install, once the
	// definition is complete.
	if o.Definition.Name == "" {
		errs = append(errs, errors.New("setup: Options.Definition.Name is required"))
	}

	if o.Definition.DrainBudget <= 0 {
		errs = append(errs, fmt.Errorf(
			"setup: Options.Definition.DrainBudget must be positive, got %s: it becomes the service's "+
				"PreshutdownTimeout, and a non-positive one would let the SCM kill a drain that was "+
				"proceeding", o.Definition.DrainBudget))
	}

	return errors.Join(errs...)
}

// ConfigFilePath is the config file this run will use: the caller's override,
// or the layout's. Exported so a caller can name it in its own output without
// re-deriving the choice and getting it subtly different.
func (o Options) ConfigFilePath() string { return o.configPath() }

// configPath is the config file this run will use: the caller's override, or
// the layout's.
func (o Options) configPath() string {
	if o.ConfigPath != "" {
		return o.ConfigPath
	}

	return o.Layout.ConfigPath
}

// verifyDeadline is the configured wait, or the measured default.
func (o Options) verifyDeadline() time.Duration {
	if o.VerifyDeadline > 0 {
		return o.VerifyDeadline
	}

	return defaultVerifyDeadline
}

// equivalent reports whether two values for a key mean the same thing, using
// the caller's test when it supplied one.
func (o Options) equivalent(key string, provisioned, resolved any) bool {
	if o.Equivalent != nil {
		return o.Equivalent(key, provisioned, resolved)
	}

	return provisioned == resolved
}

// now is the injected clock, or the real one.
func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}

	return time.Now()
}
