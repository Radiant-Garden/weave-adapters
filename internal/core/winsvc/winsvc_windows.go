//go:build windows

package winsvc

import (
	"context"
	"fmt"
	"math"
	"time"

	"golang.org/x/sys/windows/svc"
)

// IsService reports whether this process was started by the Service Control
// Manager.
//
// Detection rather than a flag: an operator cannot then start the service
// wrongly, and the console path stays byte-identical for development and for
// the smoke and e2e gates. A --service flag would be a fourth way to run the
// binary that only the SCM is allowed to use. Verified on Windows Server 2022
// to report true under the SCM and false from an elevated console.
func IsService() (bool, error) {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("determining whether this is a service: %w", err)
	}

	return is, nil
}

// Run serves under the SCM until it is told to stop, and reports the outcome.
//
// It is a translation layer and nothing else: the sequencing lives in
// Controller, which is portable and tested on every platform. The returned
// error is whatever serve returned, for the caller's own event reporting.
func Run(name string, budget time.Duration, serve ServeFunc) error {
	// The Controller is built inside Execute, not here: its Report target is
	// the status channel the SCM hands Execute, and that channel does not exist
	// until then.
	h := &handler{budget: budget, serve: serve}

	if err := svc.Run(name, h); err != nil {
		return fmt.Errorf("running service %q: %w", name, err)
	}

	return h.err
}

// handler adapts Controller to svc.Handler.
type handler struct {
	budget time.Duration
	serve  ServeFunc

	// err is what serve returned. Execute cannot return it — its signature
	// carries only the SCM's exit code — so it is stashed for Run.
	err error
}

// Execute is the SCM's entry point.
//
// The exit code is a *return value* here, not a field on a final Status: the
// svc package derives the reported Win32ExitCode and ServiceSpecificExitCode
// from what this returns and ignores those fields on any Status it is handed.
// Setting them on the Stopped report instead would report exit zero for every
// failure, and the SCM's recovery actions would never fire.
func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	// Requests are translated onto an unbuffered channel of our own, matching
	// the one x/sys hands us: its control callback blocks sending into that
	// channel from the SCM's own thread, so the pump below must not buffer away
	// the backpressure the controller is tested against.
	commands := make(chan Command)
	done := make(chan struct{})

	defer close(done)

	go func() {
		for {
			select {
			case req, ok := <-r:
				if !ok {
					return
				}

				cmd, translated := translate(req.Cmd)
				if !translated {
					// A control we never asked to accept. The SCM should not
					// send it; ignoring it is better than reporting a state
					// transition that did not happen.
					continue
				}

				select {
				case commands <- cmd:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()

	controller := New(Config{
		Serve:       h.serve,
		Report:      func(rep Report) { s <- status(rep) },
		DrainBudget: h.budget,
	})

	outcome, err := controller.Run(context.Background(), commands)
	h.err = err

	return outcome.ServiceSpecific, outcome.Code
}

// translate maps an SCM control onto this package's own command set.
//
// The default branch is the decision, not an oversight: svc.Cmd has a dozen
// members and this function exists to accept three of them, so listing the
// rest as explicit refusals would restate the default without adding anything.
//
//nolint:exhaustive // the default branch is the decision; see above.
func translate(cmd svc.Cmd) (Command, bool) {
	switch cmd {
	case svc.Interrogate:
		return CmdInterrogate, true
	case svc.Stop:
		return CmdStop, true
	case svc.PreShutdown:
		return CmdPreShutdown, true
	default:
		return 0, false
	}
}

// status renders a Report as the svc.Status the SCM expects.
//
// WaitHint is milliseconds, and it is meaningful only alongside an advancing
// CheckPoint: the SCM waits the hint for the checkpoint to move, not for the
// operation to finish.
func status(r Report) svc.Status {
	return svc.Status{
		State:      state(r.State),
		Accepts:    accepts(r.Accepts),
		CheckPoint: r.CheckPoint,
		WaitHint:   waitHintMillis(r.WaitHint),
	}
}

// waitHintMillis renders a wait hint as the milliseconds the SCM expects,
// clamping rather than wrapping.
//
// WaitHint is a uint32 of milliseconds, so it tops out near 49.7 days. A
// Duration beyond that — or a negative one out of a mis-set config — would wrap
// to a small number and tell the SCM to stop waiting almost immediately, which
// is the exact opposite of what a long hint is asking for, and it would surface
// as a drain killed seconds in.
func waitHintMillis(d time.Duration) uint32 {
	ms := d.Milliseconds()

	switch {
	case ms <= 0:
		return 0
	case ms > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(ms)
	}
}

func state(s State) svc.State {
	switch s {
	case StateStartPending:
		return svc.StartPending
	case StateRunning:
		return svc.Running
	case StateStopPending:
		return svc.StopPending
	case StateStopped:
		return svc.Stopped
	default:
		return svc.Stopped
	}
}

// accepts maps the accepted-control set.
//
// Pre-shutdown rather than shutdown: WaitToKillServiceTimeout measured 5000ms
// on Windows Server 2022, so a service accepting shutdown has its drain
// truncated on every reboot, while the pre-shutdown budget is written
// explicitly at install time.
func accepts(a Accepted) svc.Accepted {
	var out svc.Accepted

	if a&AcceptStop != 0 {
		out |= svc.AcceptStop
	}

	if a&AcceptPreShutdown != 0 {
		out |= svc.AcceptPreShutdown
	}

	return out
}
