/*
Testing: controller.go

Pending:

Tested:

	DrainDeadline -> - TestDrainDeadline_ShouldExceedTheBudgetAndMatchTheControllersOwnBound:
	                   the installer writes this as PreshutdownTimeout, which is
	                   a hard wall, so it must match the controller's own bound.

	New -> - TestNew_ShouldPanicWhenARequiredFieldIsMissing: the three wiring
	         mistakes that must not reach a running service.
	       - TestNew_ShouldDefaultTheOptionalTimings: an unset hint or tick is
	         filled in rather than left at zero, which would spin the drain.

	Controller.Run -> - TestControllerRun_ShouldReportStartPendingThenRunningWhenServeBecomesReady
	                  - TestControllerRun_ShouldNotReportRunningWhenServeFailsBeforeReady
	                  - TestControllerRun_ShouldAcceptStopAndPreShutdownOnlyWhileRunning
	                  - TestControllerRun_ShouldAnswerInterrogateWithTheCurrentStatus
	                  - TestControllerRun_ShouldCancelServeWhenAStopIsRequested
	                  - TestControllerRun_ShouldExitNonZeroWhenServeFailsWithoutAStop
	                  - TestControllerRun_ShouldExitZeroWhenServeFailsAfterARequestedStop
	                  - TestControllerRun_ShouldExitZeroWhenServeReturnsNil
	                  - TestControllerRun_ShouldAdvanceTheCheckpointWhileDraining
	                  - TestControllerRun_ShouldStayResponsiveToRequestsWhileDraining
	                  - TestControllerRun_ShouldGiveUpOnAServeThatOverrunsTheDrainBudget
	                  - TestControllerRun_ShouldTreatPreShutdownAsAStop
	                  - TestControllerRun_ShouldStopWhenAStopArrivesBeforeServeIsReady
	                  - TestControllerRun_ShouldDrainWhenAStopArrivesBeforeReadyAndServeWedges
	                  - TestControllerRun_ShouldAdvanceTheCheckpointWhenAStopArrivesBeforeReady
	                  - TestControllerRun_ShouldNotHangWhenTheRequestChannelClosesAndServeWedges

Tested elsewhere:

	That the reports reach a real SCM in the right shape, and that the SCM
	behaves as the exit-code rule assumes: measured on Windows Server 2022 in
	M4a Phase -1, and re-asserted by task service-gate. Three findings from that
	measurement are encoded here as tests rather than left as prose -- the
	non-zero exit on an unrequested failure, the zero exit on a requested one,
	and the advancing checkpoint.

	The translation of these types onto svc.ChangeRequest and svc.Status:
	winsvc_windows.go, which is compiled by build-windows and exercised by the
	gate.

Declined:

	Testing against the real svc package. It builds only on Windows, which is
	the whole reason this half of the package exists -- a test that could only
	run on the host would put the sequencing back where nobody can iterate on
	it.

	Asserting exact wall-clock durations. The timings are injected as
	configuration precisely so these tests need no sleeps proportional to the
	production values; a test that pinned real durations would be slow and
	flaky without pinning anything a caller depends on.

Additional Remarks:

	The exit-code rule is the subtlest thing in this package and the one with
	the worst failure mode in both directions. Too lax and a service that dies
	mid-life is never restarted; too strict and the SCM restarts a service the
	operator deliberately stopped. Both branches were measured against a real
	SCM before this code existed, and both are asserted below.

	Three tests wedge serve rather than letting it return on cancel, and they
	exist because a serve that returns promptly hides every missing backstop.
	The stop-before-ready path shipped broken for exactly that reason: it fell
	into the running phase, which has no ticker and no backstop, and the test
	that covered it passed because its fake serve was well behaved.

	Every test drives requests through an unbuffered channel, matching the
	Windows handler's own: the control callback blocks sending into it on the
	SCM's thread, so a controller that stops reading it stalls the SCM. Using a
	buffered channel here would hide exactly that bug.
*/
package winsvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errServe is the failure a fake serve returns when a test wants one.
var errServe = errors.New("serve failed")

// recorder collects reports. Concurrent because Run reports from its own
// goroutine while the test drives requests from another.
type recorder struct {
	mu      sync.Mutex
	reports []Report
}

func (r *recorder) record(rep Report) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.reports = append(r.reports, rep)
}

func (r *recorder) all() []Report {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Report(nil), r.reports...)
}

// states reduces the reports to their states, which is what most assertions
// are about.
func (r *recorder) states() []State {
	out := []State{}
	for _, rep := range r.all() {
		out = append(out, rep.State)
	}

	return out
}

// last returns the most recent report, failing the test if there is none. For
// assertions only -- never inside a waitFor predicate, where require would fail
// the test on the first poll instead of reporting "not yet".
func (r *recorder) last(t *testing.T) Report {
	t.Helper()

	all := r.all()
	require.NotEmpty(t, all, "no status was reported")

	return all[len(all)-1]
}

// latest is the non-failing form, for polling. The bool reports whether any
// status has been sent yet.
func (r *recorder) latest() (Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.reports) == 0 {
		return Report{}, false
	}

	return r.reports[len(r.reports)-1], true
}

// reachedRunning reports whether the controller has announced Running.
func (r *recorder) reachedRunning() bool {
	rep, ok := r.latest()

	return ok && rep.State == StateRunning
}

// waitFor blocks until cond holds, failing rather than hanging. Used instead of
// a sleep so the tests stay fast and do not encode a timing guess.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", msg)
}

// fixture is a controller wired to a recorder, with the timings shrunk so no
// test waits on a production duration.
type fixture struct {
	rec      *recorder
	requests chan Command
	outcome  Outcome
	err      error
	done     chan struct{}
}

// start runs the controller against serve and returns once it is under way.
func start(t *testing.T, serve ServeFunc, opts ...func(*Config)) *fixture {
	t.Helper()

	rec := &recorder{}
	cfg := Config{
		Serve:           serve,
		Report:          rec.record,
		DrainBudget:     50 * time.Millisecond,
		StartHint:       20 * time.Millisecond,
		CheckpointEvery: 5 * time.Millisecond,
		DrainMargin:     20 * time.Millisecond,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	// Unbuffered, like the Windows handler's own channel: a controller that
	// stops reading it must show up as a blocked send, not be absorbed.
	f := &fixture{rec: rec, requests: make(chan Command), done: make(chan struct{})}

	c := New(cfg)

	go func() {
		defer close(f.done)

		f.outcome, f.err = c.Run(t.Context(), f.requests)
	}()

	return f
}

// wait blocks until Run has returned.
func (f *fixture) wait(t *testing.T) {
	t.Helper()

	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

// serveUntilCancelled is the ordinary case: become ready, serve, stop cleanly.
func serveUntilCancelled(ctx context.Context, ready func()) error {
	ready()
	<-ctx.Done()

	return nil
}

func TestNew_ShouldPanicWhenARequiredFieldIsMissing(t *testing.T) {
	t.Parallel()

	tests := map[string]Config{
		"should panic when serve is nil": {
			Report: func(Report) {}, DrainBudget: time.Second,
		},
		"should panic when report is nil": {
			Serve: serveUntilCancelled, DrainBudget: time.Second,
		},
		"should panic when the drain budget is not positive": {
			Serve: serveUntilCancelled, Report: func(Report) {},
		},
	}

	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.Panics(t, func() { New(cfg) })
		})
	}
}

func TestNew_ShouldDefaultTheOptionalTimings(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cfg := Config{Serve: serveUntilCancelled, Report: func(Report) {}, DrainBudget: time.Second}

	// ACT
	c := New(cfg)

	// ASSERT
	assert.Positive(t, c.cfg.StartHint)
	assert.Positive(t, c.cfg.CheckpointEvery)
	assert.Positive(t, c.cfg.DrainMargin)
}

func TestControllerRun_ShouldReportStartPendingThenRunningWhenServeBecomesReady(t *testing.T) {
	t.Parallel()

	// ARRANGE
	f := start(t, serveUntilCancelled)

	// ACT
	waitFor(t, f.rec.reachedRunning, "running")

	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	assert.Equal(t,
		[]State{StateStartPending, StateRunning, StateStopPending, StateStopped},
		f.rec.states(),
	)
}

func TestControllerRun_ShouldNotReportRunningWhenServeFailsBeforeReady(t *testing.T) {
	t.Parallel()

	// ARRANGE
	serve := func(context.Context, func()) error { return errServe }

	// ACT
	f := start(t, serve)
	f.wait(t)

	// ASSERT
	// `sc start` must fail rather than succeed and then die: a bind failure is
	// exactly this shape.
	assert.NotContains(t, f.rec.states(), StateRunning)
	assert.Equal(t, []State{StateStartPending, StateStopped}, f.rec.states())
}

func TestControllerRun_ShouldAcceptStopAndPreShutdownOnlyWhileRunning(t *testing.T) {
	t.Parallel()

	// ARRANGE
	f := start(t, serveUntilCancelled)

	// ACT
	waitFor(t, f.rec.reachedRunning, "running")
	running := f.rec.last(t)

	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	// A Running that accepts nothing makes sc stop fail with 1052, and the
	// pending states must not accept a second stop mid-transition.
	assert.Equal(t, AcceptStop|AcceptPreShutdown, running.Accepts)

	for _, rep := range f.rec.all() {
		if rep.State != StateRunning {
			assert.Equal(t, Accepted(0), rep.Accepts, "%s must accept nothing", rep.State)
		}
	}
}

func TestControllerRun_ShouldAnswerInterrogateWithTheCurrentStatus(t *testing.T) {
	t.Parallel()

	// ARRANGE
	f := start(t, serveUntilCancelled)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdInterrogate

	waitFor(t, func() bool { return len(f.rec.all()) >= 3 }, "the interrogate reply")

	// ASSERT
	reply := f.rec.all()[2]
	assert.Equal(t, StateRunning, reply.State)
	assert.Equal(t, AcceptStop|AcceptPreShutdown, reply.Accepts)

	f.requests <- CmdStop

	f.wait(t)
}

func TestControllerRun_ShouldCancelServeWhenAStopIsRequested(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cancelled := make(chan struct{})
	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		close(cancelled)

		return nil
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	select {
	case <-cancelled:
	default:
		t.Fatal("serve's context was not cancelled")
	}
}

func TestControllerRun_ShouldExitNonZeroWhenServeFailsWithoutAStop(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fail := make(chan struct{})
	serve := func(_ context.Context, ready func()) error {
		ready()
		<-fail

		return errServe
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	close(fail)
	f.wait(t)

	// ASSERT
	// Measured: with SetRecoveryActionsOnNonCrashFailures set, this is what
	// makes the SCM run the restart schedule. A zero here and the service that
	// died mid-life is never restarted.
	assert.True(t, f.outcome.ServiceSpecific)
	assert.NotZero(t, f.outcome.Code)
	require.ErrorIs(t, f.err, errServe)
}

func TestControllerRun_ShouldExitZeroWhenServeFailsAfterARequestedStop(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// An overrun drain returns an error, and it must not be treated as a
	// failure to recover from.
	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()

		return errServe
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	// Measured: a non-zero code here restarts the service the operator just
	// stopped, because the non-crash-failures flag is set at install.
	assert.False(t, f.outcome.ServiceSpecific)
	assert.Zero(t, f.outcome.Code)
	assert.ErrorIs(t, f.err, errServe, "the error is still reported to the caller")
}

func TestControllerRun_ShouldExitZeroWhenServeReturnsNil(t *testing.T) {
	t.Parallel()

	// ARRANGE
	f := start(t, serveUntilCancelled)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	assert.Zero(t, f.outcome.Code)
	assert.NoError(t, f.err)
}

func TestControllerRun_ShouldAdvanceTheCheckpointWhileDraining(t *testing.T) {
	t.Parallel()

	// ARRANGE
	release := make(chan struct{})
	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		<-release

		return nil
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdStop

	// The SCM waits WaitHint for the checkpoint to MOVE, not for the service to
	// finish, so a static checkpoint is what gets a legitimate drain killed.
	waitFor(t, func() bool {
		rep, ok := f.rec.latest()

		return ok && rep.CheckPoint >= 3
	}, "the checkpoint to advance")

	close(release)
	f.wait(t)

	// ASSERT
	var pending []Report

	for _, rep := range f.rec.all() {
		if rep.State == StateStopPending {
			pending = append(pending, rep)
		}
	}

	require.GreaterOrEqual(t, len(pending), 3, "the drain reported too few checkpoints")

	for i := 1; i < len(pending); i++ {
		assert.Greater(t, pending[i].CheckPoint, pending[i-1].CheckPoint, "the checkpoint must advance")
		assert.Positive(t, pending[i].WaitHint, "a pending state needs a wait hint")
	}
}

func TestControllerRun_ShouldStayResponsiveToRequestsWhileDraining(t *testing.T) {
	t.Parallel()

	// ARRANGE
	release := make(chan struct{})
	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		<-release

		return nil
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	f.requests <- CmdStop

	// ACT
	// An unbuffered send that completes IS the assertion: the Windows control
	// callback blocks here on the SCM's own thread, so a controller that stops
	// reading during the drain freezes services.msc.
	select {
	case f.requests <- CmdInterrogate:
	case <-time.After(time.Second):
		t.Fatal("the controller stopped reading requests during the drain")
	}

	close(release)
	f.wait(t)

	// ASSERT
	assert.Equal(t, StateStopped, f.rec.last(t).State)
}

func TestControllerRun_ShouldGiveUpOnAServeThatOverrunsTheDrainBudget(t *testing.T) {
	t.Parallel()

	// ARRANGE
	wedged := make(chan struct{})

	t.Cleanup(func() { close(wedged) })

	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		<-wedged // never, within the test's lifetime

		return nil
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	// Zero, not non-zero: the operator asked for a stop and got one. The wedged
	// goroutine is a defect in serve, reported through the error rather than by
	// having the SCM restart a service nobody wants running.
	assert.Zero(t, f.outcome.Code)
	require.ErrorIs(t, f.err, ErrServeOverran)
	assert.Equal(t, StateStopped, f.rec.last(t).State)
}

func TestControllerRun_ShouldTreatPreShutdownAsAStop(t *testing.T) {
	t.Parallel()

	// ARRANGE
	f := start(t, serveUntilCancelled)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	f.requests <- CmdPreShutdown

	f.wait(t)

	// ASSERT
	assert.Equal(t,
		[]State{StateStartPending, StateRunning, StateStopPending, StateStopped},
		f.rec.states(),
	)
	assert.Zero(t, f.outcome.Code)
}

func TestControllerRun_ShouldStopWhenAStopArrivesBeforeServeIsReady(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// An operator who cancels a slow start should not have to wait for the
	// start to finish first.
	serve := func(ctx context.Context, ready func()) error {
		<-ctx.Done()
		ready() // late, and must neither block nor panic

		return nil
	}

	f := start(t, serve)

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	assert.NotContains(t, f.rec.states(), StateRunning)
	assert.Equal(t, StateStopped, f.rec.last(t).State)
	assert.Zero(t, f.outcome.Code)
}

func TestControllerRun_ShouldDrainWhenAStopArrivesBeforeReadyAndServeWedges(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The stop-before-ready path used to fall into the running phase, which has
	// neither the checkpoint ticker nor the backstop. A serve that returned
	// promptly on cancel hid it; one that wedges does not.
	wedged := make(chan struct{})

	t.Cleanup(func() { close(wedged) })

	serve := func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		<-wedged // never signals ready, and never returns

		return nil
	}

	f := start(t, serve)

	// ACT
	f.requests <- CmdStop

	f.wait(t)

	// ASSERT
	// Without the fix this hangs forever, which under a real SCM is a service
	// parked at STOP_PENDING until someone kills the process.
	require.ErrorIs(t, f.err, ErrServeOverran)
	assert.Zero(t, f.outcome.Code, "a requested stop exits zero even when serve wedged")
	assert.Equal(t, StateStopped, f.rec.last(t).State)
	assert.NotContains(t, f.rec.states(), StateRunning)
}

func TestControllerRun_ShouldAdvanceTheCheckpointWhenAStopArrivesBeforeReady(t *testing.T) {
	t.Parallel()

	// ARRANGE
	release := make(chan struct{})
	serve := func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		<-release

		return nil
	}

	f := start(t, serve)

	// ACT
	f.requests <- CmdStop

	// The SCM needs the checkpoint to move on this path too, or it stops
	// waiting on a drain that began before the service ever reported Running.
	waitFor(t, func() bool {
		rep, ok := f.rec.latest()

		return ok && rep.State == StateStopPending && rep.CheckPoint >= 3
	}, "the checkpoint to advance on the stop-before-ready path")

	close(release)
	f.wait(t)

	// ASSERT
	assert.Equal(t, StateStopped, f.rec.last(t).State)
	assert.Zero(t, f.outcome.Code)
}

func TestControllerRun_ShouldNotHangWhenTheRequestChannelClosesAndServeWedges(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The SCM never closes the channel, so this guards the one path that
	// bypasses the draining loop rather than a production scenario.
	wedged := make(chan struct{})

	t.Cleanup(func() { close(wedged) })

	serve := func(ctx context.Context, ready func()) error {
		ready()
		<-ctx.Done()
		<-wedged

		return nil
	}

	f := start(t, serve)
	waitFor(t, f.rec.reachedRunning, "running")

	// ACT
	close(f.requests)
	f.wait(t)

	// ASSERT
	require.ErrorIs(t, f.err, ErrServeOverran)
	assert.Zero(t, f.outcome.Code)
	assert.Equal(t, StateStopped, f.rec.last(t).State)
}

func TestDrainDeadline_ShouldExceedTheBudgetAndMatchTheControllersOwnBound(t *testing.T) {
	t.Parallel()

	// ARRANGE
	budget := 15 * time.Second

	// ACT
	deadline := DrainDeadline(budget)

	// ASSERT
	// Three consumers need this number and only one of them starts from the
	// budget alone: the server drains within the budget, the controller
	// reports this as its WaitHint and fires its backstop here, and the
	// installer writes it as the service's PreshutdownTimeout -- which is a
	// hard wall. Writing the bare budget there would have the SCM stop waiting
	// at the exact moment a drain that used its whole budget reports Stopped.
	assert.Greater(t, deadline, budget)

	// The same arithmetic New applies when a caller sets no margin, which is
	// what makes "derived from one value" true rather than aspirational.
	c := New(Config{
		Serve:       serveUntilCancelled,
		Report:      func(Report) {},
		DrainBudget: budget,
	})
	assert.Equal(t, deadline, c.cfg.DrainBudget+c.cfg.DrainMargin)
}
