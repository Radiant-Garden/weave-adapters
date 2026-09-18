package winsvc

import (
	"errors"
	"fmt"
	"time"
)

// Recovery schedule defaults. Three restarts at widening intervals, then the
// counter clears after a day.
//
// The widening matters: a transient cause (a backend that was briefly
// unreachable at boot) clears in the first few seconds, while a real one — a
// bad config, a missing token store — would otherwise restart-loop every five
// seconds forever, filling the Event Log faster than anyone can read it. The
// reset period is what stops the third interval becoming permanent: without
// it the failure counter never clears, so a service that failed three times
// last March is still on the 60-second schedule today.
var (
	// DefaultRecovery is the restart schedule install registers.
	DefaultRecovery = []time.Duration{5 * time.Second, 10 * time.Second, 60 * time.Second}

	// DefaultResetPeriod is how long without a failure clears the counter.
	DefaultResetPeriod = 24 * time.Hour
)

// ErrNotInstalled reports that the named service is not registered. It is
// returned rather than an opaque SCM error so `uninstall` and `stop` can treat
// an already-absent service as success and be safely re-run.
var ErrNotInstalled = errors.New("winsvc: the service is not installed")

// ErrAlreadyInstalled reports that the name is already registered. Install
// refuses rather than reconfiguring, because silently adopting a registration
// somebody else made is how two adapters end up sharing one name.
var ErrAlreadyInstalled = errors.New("winsvc: the service is already installed")

// Definition is everything Install registers. It is a value rather than a set
// of parameters because the binary composes it and this package must not know
// which adapter it belongs to.
type Definition struct {
	// Name is the SCM service name and the Event Log source name.
	Name string
	// DisplayName and Description are what services.msc shows.
	DisplayName string
	Description string

	// BinPath is the absolute path to the executable, and Args are its
	// arguments — both unescaped.
	//
	// DO NOT QUOTE EITHER. The SCM registration escapes the path and every
	// argument already, so pre-quoting produces a doubly-escaped ImagePath and
	// a service that cannot start. Verified on Windows Server 2022: a spaced
	// path and two spaced argument values round-tripped through the registry
	// correctly with no quoting here.
	BinPath string
	Args    []string

	// DrainBudget is written as the service's PreshutdownTimeout, so the SCM
	// allows at least as long as the server actually takes to drain. It is the
	// same value the HTTP server drains within — the binary owns it and passes
	// it to both.
	DrainBudget time.Duration

	// Recovery is the restart schedule, and ResetPeriod is how long without a
	// failure clears the counter. Zero values take the defaults above.
	Recovery    []time.Duration
	ResetPeriod time.Duration
}

// Validate reports whether the definition can be registered.
func (d Definition) Validate() error {
	var errs []error

	if d.Name == "" {
		errs = append(errs, errors.New("winsvc: Definition.Name is required"))
	}

	if d.BinPath == "" {
		errs = append(errs, errors.New("winsvc: Definition.BinPath is required"))
	}

	if d.DrainBudget <= 0 {
		errs = append(errs, fmt.Errorf(
			"winsvc: Definition.DrainBudget must be positive, got %s: it becomes the service's "+
				"PreshutdownTimeout, and a non-positive one would let the SCM kill a drain that was proceeding",
			d.DrainBudget,
		))
	}

	return errors.Join(errs...)
}

// withDefaults returns the definition with the zero-valued recovery settings
// filled in.
func (d Definition) withDefaults() Definition {
	if len(d.Recovery) == 0 {
		d.Recovery = DefaultRecovery
	}

	if d.ResetPeriod <= 0 {
		d.ResetPeriod = DefaultResetPeriod
	}

	return d
}

// ServiceStatus is what the SCM knows about a registered service.
type ServiceStatus struct {
	Name      string
	Installed bool

	// State is the current lifecycle state. Meaningless when Installed is
	// false.
	State State
	// StartType is rendered for a human: "automatic", "automatic (delayed)",
	// "manual" or "disabled".
	StartType string
	// BinPath is the registered ImagePath, escaping included — it is shown
	// verbatim because a mangled one is exactly what an operator is looking
	// for when a service will not start.
	BinPath string
	// Command is the same ImagePath split back into the executable and its
	// arguments, unescaped.
	//
	// It exists because BinPath cannot be compared against a Definition. The
	// registration escapes the path and every argument (syscall.EscapeArg, via
	// CreateService), while a Definition holds the unescaped values — so
	// checking whether a registered service matches a desired one against
	// BinPath would need a portable re-implementation of that escaping, which
	// is a rule that is wrong in exactly the cases that matter: a path with a
	// space, a quote, a trailing backslash.
	//
	// Windows fills it with DecomposeCommandLine, which is the inverse Windows
	// itself applies. Off Windows it is nil, and a caller comparing against
	// nil sees a mismatch rather than a false match.
	Command []string
	// PreshutdownTimeout is what the SCM will allow the drain at reboot.
	PreshutdownTimeout time.Duration
	// RestartsOnCleanExit reports the failure-actions flag.
	//
	// Surfaced because its absence is invisible otherwise, and it is the
	// difference between a configured restart schedule and an inert one:
	// without it the SCM runs recovery actions only when the process dies
	// without reporting Stopped, which is not how a config failure exits.
	RestartsOnCleanExit bool
}

// Manager is the set of SCM operations the service subcommand needs.
//
// An interface, so the command's own logic — flag parsing, the consent gate,
// the refusals, the output — is tested on any platform. The real one talks to
// the SCM and exists only on Windows, which would otherwise put every one of
// those rules behind a host nobody can iterate on.
type Manager interface {
	// Install registers the service. It returns ErrAlreadyInstalled rather
	// than reconfiguring an existing registration.
	Install(d Definition) error
	// Uninstall stops and deregisters the service, and removes its Event Log
	// source. An absent service is not an error.
	Uninstall(name string) error
	// Start starts an installed service and waits for it to report Running.
	Start(name string) error
	// Stop stops a running service and waits for it to report Stopped. A
	// service that is already stopped is not an error.
	Stop(name string) error
	// Status reports what the SCM knows. An absent service yields a
	// ServiceStatus with Installed false and no error.
	Status(name string) (ServiceStatus, error)
	// Close releases the SCM connection.
	Close() error
}
