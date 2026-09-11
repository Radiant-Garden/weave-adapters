//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"math"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// transitionPoll is how often Start and Stop re-read the service state, and
// transitionTimeout bounds the wait.
//
// The timeout exceeds the pre-shutdown budget a definition can ask for, so a
// legitimate slow drain is never reported as a failed stop by this side.
const (
	transitionPoll    = 250 * time.Millisecond
	transitionTimeout = 4 * time.Minute
)

// maxPreshutdownMillis bounds what the status output will believe from the
// registry. A day is already far past any drain worth configuring.
const maxPreshutdownMillis = uint64(24 * 60 * 60 * 1000)

// scmManager talks to the real Service Control Manager.
type scmManager struct{ m *mgr.Mgr }

// NewManager connects to the SCM. The connection needs Administrator, so the
// error says so rather than surfacing a bare access-denied.
func NewManager() (Manager, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("connecting to the service control manager "+
			"(this needs an elevated prompt — run as Administrator): %w", err)
	}

	return &scmManager{m: m}, nil
}

// Close releases the SCM handle.
func (s *scmManager) Close() error {
	if err := s.m.Disconnect(); err != nil {
		return fmt.Errorf("disconnecting from the service control manager: %w", err)
	}

	return nil
}

// open returns the named service, or ErrNotInstalled.
func (s *scmManager) open(name string) (*mgr.Service, error) {
	svcHandle, err := s.m.OpenService(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}

	return svcHandle, nil
}

// Install registers the service with the recovery configuration the milestone
// measured into place.
func (s *scmManager) Install(d Definition) error {
	if err := d.Validate(); err != nil {
		return err
	}

	d = d.withDefaults()

	if existing, err := s.m.OpenService(d.Name); err == nil {
		_ = existing.Close()

		return fmt.Errorf("%w: %s", ErrAlreadyInstalled, d.Name)
	}

	// Args are passed through unescaped: CreateService runs syscall.EscapeArg
	// over the path and every argument itself, and pre-quoting here produces a
	// doubly-escaped ImagePath.
	created, err := s.m.CreateService(d.Name, d.BinPath, mgr.Config{
		DisplayName: d.DisplayName,
		Description: d.Description,
		StartType:   mgr.StartAutomatic,
		// Not delayed. Measured on Windows Server 2022: the local DHCP Server
		// service reaches Running about four seconds after we do, so delaying
		// would trade a four-second window in which health honestly answers
		// 503 for roughly two minutes with no adapter at all.
		DelayedAutoStart: false,
	}, d.Args...)
	if err != nil {
		return fmt.Errorf("creating the service %q: %w", d.Name, err)
	}

	defer func() { _ = created.Close() }()

	if err := s.configureRecovery(created, d); err != nil {
		// Leaving a half-configured service behind would be worse than not
		// installing: the restart schedule an operator can see in services.msc
		// would not be the one in force.
		_ = created.Delete()

		return err
	}

	if err := setPreshutdownTimeout(d.Name, d.DrainBudget); err != nil {
		_ = created.Delete()

		return err
	}

	if err := InstallEventLogSource(d.Name); err != nil {
		_ = created.Delete()

		return err
	}

	return nil
}

// configureRecovery installs the restart schedule and — the part without which
// it never fires — the failure-actions flag.
func (s *scmManager) configureRecovery(service *mgr.Service, d Definition) error {
	actions := make([]mgr.RecoveryAction, 0, len(d.Recovery))
	for _, delay := range d.Recovery {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: delay})
	}

	if err := service.SetRecoveryActions(actions, resetSeconds(d.ResetPeriod)); err != nil {
		return fmt.Errorf("setting the recovery actions for %q: %w", d.Name, err)
	}

	// The measured half. Without this flag the SCM runs recovery actions only
	// when the process terminates WITHOUT reporting SERVICE_STOPPED — which is
	// not how a config failure exits, so the schedule above would be inert
	// while looking configured in services.msc. Verified on Windows Server
	// 2022: with the flag set, a clean Stopped carrying exit code 1 produced
	// restarts at 5s, 10s and 60s; the service stayed stopped on exit 0.
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enabling recovery on clean-exit failures for %q: %w", d.Name, err)
	}

	return nil
}

// resetSeconds renders a reset period as the seconds the SCM expects,
// clamping rather than wrapping. A wrapped value would silently shorten the
// window in which failures accumulate, so a service failing every few minutes
// would never reach its second recovery action.
func resetSeconds(d time.Duration) uint32 {
	secs := d.Seconds()

	switch {
	case secs <= 0:
		return 0
	case secs > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(secs)
	}
}

// Uninstall stops and deregisters the service, and removes its Event Log
// source. An absent service is not an error, so an interrupted uninstall can
// be re-run.
func (s *scmManager) Uninstall(name string) error {
	service, err := s.open(name)
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			// Still remove the source: the two are registered separately, so
			// an uninstall interrupted between them leaves one behind.
			return RemoveEventLogSource(name)
		}

		return err
	}

	// Stopped first. Deleting a running service marks it for deletion and
	// leaves it registered until the last handle closes, which is the 1072 an
	// operator then hits on the next install.
	if err := s.stopAndWait(service, name); err != nil {
		_ = service.Close()

		return err
	}

	delErr := service.Delete()

	// Closed before returning, deliberately: Windows removes a service only
	// once every handle to it is gone, so holding this one is what turns a
	// successful Delete into a name that still exists.
	closeErr := service.Close()

	if delErr != nil {
		return fmt.Errorf("deleting the service %q: %w", name, delErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing the service handle for %q: %w", name, closeErr)
	}

	return RemoveEventLogSource(name)
}

// Start starts the service and waits for Running.
func (s *scmManager) Start(name string) error {
	service, err := s.open(name)
	if err != nil {
		return err
	}

	defer func() { _ = service.Close() }()

	if err := service.Start(); err != nil {
		return fmt.Errorf("starting the service %q: %w", name, err)
	}

	return waitForState(service, name, svc.Running)
}

// Stop stops the service and waits for Stopped.
func (s *scmManager) Stop(name string) error {
	service, err := s.open(name)
	if err != nil {
		return err
	}

	defer func() { _ = service.Close() }()

	return s.stopAndWait(service, name)
}

// stopAndWait sends a stop unless the service is already stopped, then waits.
func (s *scmManager) stopAndWait(service *mgr.Service, name string) error {
	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("querying the service %q: %w", name, err)
	}

	if status.State == svc.Stopped {
		return nil
	}

	if _, err := service.Control(svc.Stop); err != nil {
		// Already stopping, or stopped between the query and here. Not a
		// failure: what the caller asked for is what is happening.
		if !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stopping the service %q: %w", name, err)
		}
	}

	return waitForState(service, name, svc.Stopped)
}

// waitForState polls until the service reaches want, or the timeout expires.
//
// Polled rather than event-driven because the SCM offers no completion
// callback, and the drain is legitimately long: the wait has to outlast the
// pre-shutdown budget or this side would report a failure for a stop that was
// proceeding correctly.
func waitForState(service *mgr.Service, name string, want svc.State) error {
	deadline := time.Now().Add(transitionTimeout)

	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("querying the service %q: %w", name, err)
		}

		if status.State == want {
			return nil
		}

		time.Sleep(transitionPoll)
	}

	return fmt.Errorf("the service %q did not reach the expected state within %s", name, transitionTimeout)
}

// Status reports what the SCM knows. An absent service is not an error.
func (s *scmManager) Status(name string) (ServiceStatus, error) {
	service, err := s.m.OpenService(name)
	if err != nil {
		return ServiceStatus{Name: name, Installed: false}, nil
	}

	defer func() { _ = service.Close() }()

	out := ServiceStatus{Name: name, Installed: true}

	status, err := service.Query()
	if err != nil {
		return out, fmt.Errorf("querying the service %q: %w", name, err)
	}

	out.State = fromSvcState(status.State)

	cfg, err := service.Config()
	if err != nil {
		return out, fmt.Errorf("reading the configuration of %q: %w", name, err)
	}

	out.StartType = startTypeName(cfg.StartType, cfg.DelayedAutoStart)
	out.BinPath = cfg.BinaryPathName

	// Reported even though nothing else reads it: its absence is the
	// difference between a restart schedule that fires and one that does not,
	// and there is no other way for an operator to see which they have.
	if flag, err := service.RecoveryActionsOnNonCrashFailures(); err == nil {
		out.RestartsOnCleanExit = flag
	}

	out.PreshutdownTimeout = preshutdownTimeout(name)

	return out, nil
}

// fromSvcState maps the SCM's state onto this package's own.
//
// The pause states are absent because the handler never accepts pause, so the
// SCM cannot put the service into one. Listing them would claim a transition
// this service can make.
//
//nolint:exhaustive // the pause states are unreachable; see above.
func fromSvcState(s svc.State) State {
	switch s {
	case svc.StartPending:
		return StateStartPending
	case svc.Running:
		return StateRunning
	case svc.StopPending:
		return StateStopPending
	case svc.Stopped:
		return StateStopped
	default:
		return 0
	}
}

// startTypeName renders the start type for a human.
func startTypeName(startType uint32, delayed bool) string {
	switch startType {
	case mgr.StartAutomatic:
		if delayed {
			return "automatic (delayed)"
		}

		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// serviceKeyPath is where a service's own registry values live.
func serviceKeyPath(name string) string {
	return `SYSTEM\CurrentControlSet\Services\` + name
}

// setPreshutdownTimeout writes the service's pre-shutdown budget.
//
// mgr does not wrap this, so it is a plain REG_DWORD. It is written rather
// than left to the SCM default because the default is not ours to rely on:
// measured settable on Windows Server 2022, which is what lets the drain
// budget the binary owns reach the SCM instead of being hoped for.
func setPreshutdownTimeout(name string, d time.Duration) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath(name), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the registry key for %q: %w", name, err)
	}

	defer func() { _ = k.Close() }()

	//nolint:gosec // G115: a drain budget in milliseconds, validated positive and operator-scaled.
	if err := k.SetDWordValue("PreshutdownTimeout", uint32(d.Milliseconds())); err != nil {
		return fmt.Errorf("setting PreshutdownTimeout for %q: %w", name, err)
	}

	return nil
}

// preshutdownTimeout reads the budget back, reporting zero when the value is
// absent and the SCM default therefore applies.
func preshutdownTimeout(name string) time.Duration {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKeyPath(name), registry.QUERY_VALUE)
	if err != nil {
		return 0
	}

	defer func() { _ = k.Close() }()

	ms, _, err := k.GetIntegerValue("PreshutdownTimeout")
	if err != nil {
		return 0
	}

	// Clamped: the value is whatever is in the registry, and a hand-edited one
	// large enough to overflow would render as a negative duration in the
	// status output -- which reads as a bug in this tool rather than in the
	// registry entry it is reporting.
	if ms > maxPreshutdownMillis {
		return time.Duration(maxPreshutdownMillis) * time.Millisecond
	}

	return time.Duration(ms) * time.Millisecond
}
