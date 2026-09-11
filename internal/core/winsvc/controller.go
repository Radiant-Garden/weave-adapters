// Package winsvc runs the adapter under the Windows Service Control Manager.
//
// It is platform-specific but adapter-agnostic: it imports no adapter, and it
// would stop compiling for none of them. The package is split deliberately.
// This file is the portable half — the sequencing the SCM protocol requires,
// expressed over this package's own types, with no build tag and no dependency
// on golang.org/x/sys. The Windows half is a translation of svc.ChangeRequest
// and svc.Status onto it.
//
// The split is not stylistic. golang.org/x/sys/windows/svc builds only on
// Windows, so a state machine written directly against svc.Handler.Execute
// could be tested only on a Windows host. Every subtle rule here — when Running
// may be reported, which exit code a failure yields, that the request channel
// must stay served during the drain — is sequencing rather than Win32 API, and
// belongs where the ordinary test gate exercises it.
//
// The rules encoded below were measured against a real SCM on Windows Server
// 2022 before this code was written; see .claude/plans/M4a-windows-service.md,
// Phase -1.
package winsvc

import (
	"context"
	"errors"
	"time"
)

// State is a service lifecycle state, mirroring the SCM's own. It is declared
// here rather than aliased from x/sys so this file stays portable.
type State int

// The states this controller reports. The SCM defines others (paused,
// continuing) that a service which does not accept pause never enters, so they
// are deliberately absent rather than declared and unreachable.
const (
	StateStartPending State = iota + 1
	StateRunning
	StateStopPending
	StateStopped
)

// String renders a state for test failures and diagnostics.
func (s State) String() string {
	switch s {
	case StateStartPending:
		return "StartPending"
	case StateRunning:
		return "Running"
	case StateStopPending:
		return "StopPending"
	case StateStopped:
		return "Stopped"
	default:
		return "unknown"
	}
}

// Command is a control request from the SCM.
type Command int

// The commands this controller answers. Pause and continue are absent for the
// reason the corresponding states are.
const (
	CmdInterrogate Command = iota + 1
	CmdStop
	CmdPreShutdown
)

// Accepted is the set of controls a reported state accepts, as a bitmask
// because that is the shape the SCM uses.
type Accepted uint32

// The controls this service accepts while running.
//
// AcceptPreShutdown rather than a shutdown bit: WaitToKillServiceTimeout
// measured 5000ms on Windows Server 2022, so a service accepting shutdown has
// its drain truncated on every reboot, while pre-shutdown's budget is set
// explicitly at install time.
const (
	AcceptStop Accepted = 1 << iota
	AcceptPreShutdown
)

// Report is one status report to the SCM.
//
// CheckPoint and WaitHint are meaningful only in a pending state. A steady
// state reports both as zero, which is what the SCM itself displays.
type Report struct {
	State      State
	Accepts    Accepted
	CheckPoint uint32
	WaitHint   time.Duration
}

// Outcome is what the SCM is told about why the service stopped.
//
// These are return values of the Windows handler's Execute, not fields on a
// final status: the svc package derives the reported Win32ExitCode and
// ServiceSpecificExitCode from what Execute returned and ignores the
// corresponding fields on any Status it is handed.
type Outcome struct {
	// ServiceSpecific selects ERROR_SERVICE_SPECIFIC_ERROR as the Win32 code,
	// with Code carried as the service-specific one.
	ServiceSpecific bool
	// Code is zero for every clean stop. It is non-zero only when serve failed
	// without a stop having been requested — see Run.
	Code uint32
}

// ErrServeOverran reports that serve did not return within the drain budget
// after its context was cancelled. The process is about to be abandoned with
// that goroutine still running, which is a defect in serve rather than a
// condition to recover from.
var ErrServeOverran = errors.New("serve did not return within the drain budget")

// ServeFunc is the work the service exists to do. It runs until ctx is
// cancelled and calls ready exactly once, as soon as it is actually serving.
//
// The callback is what makes Running mean "bound and answering". Reporting it
// when the goroutine merely starts makes `sc start` succeed for a service that
// dies a millisecond later on a port conflict.
type ServeFunc func(ctx context.Context, ready func()) error

// ReportFunc delivers a status report to the SCM. A plain function rather than
// an interface: there is one method, and the Windows half is a two-line
// translation onto a channel send.
type ReportFunc func(Report)

// Defaults for the timings a caller does not set.
const (
	defaultStartHint       = 15 * time.Second
	defaultCheckpointEvery = 2 * time.Second
	defaultDrainMargin     = 5 * time.Second
)

// Config is what Controller needs. Serve, Report and DrainBudget are required;
// the rest default.
type Config struct {
	// Serve is the work. Required.
	Serve ServeFunc
	// Report delivers status to the SCM. Required.
	Report ReportFunc
	// DrainBudget is how long serve may take to return after its context is
	// cancelled. It must be the same value the HTTP server drains within, which
	// is why it arrives as configuration rather than as a constant here: the
	// binary owns it and passes it to both.
	DrainBudget time.Duration

	// StartHint bounds startup in the WaitHint reported while starting.
	StartHint time.Duration
	// CheckpointEvery is how often the drain advances its checkpoint. The SCM
	// waits WaitHint for the checkpoint to *move*, not for the service to
	// finish, so a drain that never advances it is cut short regardless of how
	// generous the hint was.
	CheckpointEvery time.Duration
	// DrainMargin is added to DrainBudget before the backstop fires, so a serve
	// that is draining correctly is never cut off by this package.
	DrainMargin time.Duration
}

// Controller sequences the SCM protocol around a ServeFunc.
type Controller struct {
	cfg Config
}

// New returns a Controller. It panics on a missing required field, which is a
// wiring mistake rather than an operator one: there is no input that causes it
// and no runtime handling that would help, and every call site runs at startup.
func New(cfg Config) *Controller {
	switch {
	case cfg.Serve == nil:
		panic("winsvc: Config.Serve is required")
	case cfg.Report == nil:
		panic("winsvc: Config.Report is required")
	case cfg.DrainBudget <= 0:
		panic("winsvc: Config.DrainBudget must be positive")
	}

	if cfg.StartHint <= 0 {
		cfg.StartHint = defaultStartHint
	}

	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = defaultCheckpointEvery
	}

	if cfg.DrainMargin <= 0 {
		cfg.DrainMargin = defaultDrainMargin
	}

	return &Controller{cfg: cfg}
}

// session is one Run in progress. It exists so the three phases can share the
// channels and the running status without threading six parameters each.
type session struct {
	c        *Controller
	requests <-chan Command
	ready    <-chan struct{}
	result   <-chan error
	cancel   context.CancelFunc

	last    Report
	stopped bool // a Stop or PreShutdown was requested
}

// Run drives the service lifecycle to completion and reports the outcome.
//
// The exit code is narrower than "serve failed". A stop the operator asked for
// is never a failure, even when the drain overran, because the SCM's recovery
// actions fire on a non-zero exit once install has set the non-crash-failures
// flag — so a non-zero code here restarts the service the operator just
// stopped. Measured on Windows Server 2022: with that flag set, exit 1 after a
// clean Stopped triggered the full 5s/10s/60s restart schedule, while exit 0
// left the service stopped.
//
//	serve returned          stop requested first    exit
//	error                   no                      non-zero
//	error                   yes                     zero
//	nil                     either                  zero
//
// The returned error is whatever serve returned, or ErrServeOverran. It is for
// the caller's own event reporting; the SCM sees only the Outcome.
func (c *Controller) Run(ctx context.Context, requests <-chan Command) (Outcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ready := make(chan struct{})
	result := make(chan error, 1)

	// Buffered and closed by the only writer, so a serve that signals ready
	// after a stop already cancelled it neither blocks nor panics.
	readyOnce := make(chan struct{}, 1)

	go func() {
		result <- c.cfg.Serve(ctx, func() {
			select {
			case readyOnce <- struct{}{}:
				close(ready)
			default:
			}
		})
	}()

	s := &session{c: c, requests: requests, ready: ready, result: result, cancel: cancel}

	s.report(Report{State: StateStartPending, CheckPoint: 1, WaitHint: c.cfg.StartHint})

	if done, outcome, err := s.starting(); done {
		return outcome, err
	}

	if done, outcome, err := s.running(); done {
		return outcome, err
	}

	return s.draining()
}

// starting waits for serve to become ready. A stop that arrives first is
// honoured: an operator who cancels a slow start should not have to wait for it
// to finish before the service will stop.
func (s *session) starting() (bool, Outcome, error) {
	for {
		select {
		case <-s.ready:
			s.report(Report{State: StateRunning, Accepts: AcceptStop | AcceptPreShutdown})

			return false, Outcome{}, nil

		case serveErr := <-s.result:
			// serve returned without ever becoming ready: a bind failure, or a
			// config error that surfaced late. Running was never reported, so
			// `sc start` fails, which is the honest answer.
			outcome, err := s.finish(serveErr)

			return true, outcome, err

		case cmd, ok := <-s.requests:
			if !ok {
				outcome, err := s.abandon()

				return true, outcome, err
			}

			if s.control(cmd) {
				return false, Outcome{}, nil
			}
		}
	}
}

// running serves until serve returns on its own or a stop is requested.
func (s *session) running() (bool, Outcome, error) {
	for {
		select {
		case serveErr := <-s.result:
			// serve died while serving. Nobody asked it to, so this is the one
			// path that yields a non-zero exit and lets the SCM restart us.
			outcome, err := s.finish(serveErr)

			return true, outcome, err

		case cmd, ok := <-s.requests:
			if !ok {
				outcome, err := s.abandon()

				return true, outcome, err
			}

			if s.control(cmd) {
				return false, Outcome{}, nil
			}
		}
	}
}

// draining waits for serve to return after its context was cancelled,
// advancing the checkpoint so the SCM keeps waiting, and staying responsive to
// further requests throughout.
//
// Both properties are load-bearing and were measured rather than assumed. The
// SCM waits WaitHint for the checkpoint to advance: a 30-second drain that
// ticked every two seconds completed untouched, twice. And the Windows
// handler's control callback blocks sending into an unbuffered channel on the
// SCM's own thread, so a controller that waited only on serve would stall the
// next Interrogate for the length of the drain.
func (s *session) draining() (Outcome, error) {
	ticker := time.NewTicker(s.c.cfg.CheckpointEvery)
	defer ticker.Stop()

	backstop := time.NewTimer(s.c.cfg.DrainBudget + s.c.cfg.DrainMargin)
	defer backstop.Stop()

	for {
		select {
		case err := <-s.result:
			return s.finish(err)

		case <-ticker.C:
			next := s.last
			next.CheckPoint++
			s.report(next)

		case <-backstop.C:
			// serve is wedged. The stop was requested, so the operator gets the
			// stop they asked for and the exit stays zero; the abandoned
			// goroutine is reported through the error instead.
			return s.finish(ErrServeOverran)

		case cmd, ok := <-s.requests:
			if !ok {
				return s.abandon()
			}

			s.control(cmd)
		}
	}
}

// control answers one request, reporting whether it began the drain.
func (s *session) control(cmd Command) (draining bool) {
	switch cmd {
	case CmdInterrogate:
		// Re-report the current status. The SCM asks this whenever someone
		// refreshes services.msc, including mid-drain.
		s.report(s.last)

		return false

	case CmdStop, CmdPreShutdown:
		if s.stopped {
			return true
		}

		s.stopped = true
		s.cancel()

		s.report(Report{
			State:      StateStopPending,
			CheckPoint: 1,
			WaitHint:   s.c.cfg.DrainBudget + s.c.cfg.DrainMargin,
		})

		return true

	default:
		return s.stopped
	}
}

// finish reports Stopped and resolves the exit code. See Run for the rule.
func (s *session) finish(err error) (Outcome, error) {
	s.report(Report{State: StateStopped})

	if err != nil && !s.stopped {
		return Outcome{ServiceSpecific: true, Code: 1}, err
	}

	return Outcome{}, err
}

// abandon handles a closed request channel, which the SCM never does. Treated
// as a stop rather than spun on, because a nil-yielding select would otherwise
// busy-loop.
func (s *session) abandon() (Outcome, error) {
	if !s.stopped {
		s.stopped = true
		s.cancel()
	}

	return s.finish(<-s.result)
}

// report sends a status and remembers it, so Interrogate can answer with the
// current one rather than a reconstruction.
func (s *session) report(r Report) {
	s.last = r
	s.c.cfg.Report(r)
}
