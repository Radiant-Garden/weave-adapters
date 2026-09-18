// Package setup provisions an adapter as a Windows service.
//
// It holds the steps an operator would otherwise run by hand in the right
// order with the right values: resolve and validate the configuration the way
// the server will, refuse a binary directory an unprivileged account can
// write, register the service, and lock down the files the configuration
// names.
//
// Why it is in core, given that every one of those steps is about one
// adapter's deployment: nothing here is spelled in terms of DHCP. What the
// caller supplies is values — a config.Spec, a winsvc.Definition template, a
// binary path, and a Validate function — the same way it supplies routes to
// httpserver. The one genuinely adapter-bound part, the adapter's own
// configuration check, arrives as InstallOptions.Validate precisely because
// core must never import internal/adapters.
//
// It performs no terminal I/O. Every step reports what it did as data and the
// caller renders it, because the callers this is shaped for are not all
// terminals: the service subcommand prints, M4b's `setup` verb prints
// differently, and an MSI custom action reads an exit code.
package setup

import (
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// configFlag is the loader's own flag for the config file. Spelled once,
// because every resolution in this package has to hand the loader the same
// argument and a typo would silently resolve the defaults instead.
const configFlag = "--config"

// ManagerFactory opens an SCM connection.
type ManagerFactory func() (winsvc.Manager, error)

// SecureFunc applies the file lockdown.
type SecureFunc func([]winsvc.Securable) ([]winsvc.SecureResult, error)

// CheckDirFunc reports whether a directory grants write beyond the policy.
type CheckDirFunc func(dir string) error

// Deps are the platform operations these steps need.
//
// Injected, all three, so the decisions — which configuration is refused,
// which paths get secured, what gets registered, in what order — are tested on
// any platform rather than only on a host nobody can iterate on. The decisions
// belong to this package; only the syscalls are the platform's.
//
// internal/core/winsvc/winsvctest supplies doubles for the first two.
type Deps struct {
	NewManager ManagerFactory
	Secure     SecureFunc
	CheckDir   CheckDirFunc

	// Get performs one HTTP GET, for the verification step. Injected like the
	// rest: verification's decisions — what counts as reachable, which status
	// proves the token store was read, what an unhealthy component means — are
	// this package's, and testing them against a real listener would make them
	// slow and flaky rather than more true.
	Get GetFunc
}
