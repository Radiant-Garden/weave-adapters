//go:build windows

/*
Testing: winsvc_windows.go

Pending:

Tested:

	translate -> - TestTranslate_ShouldMapTheControlsWeAccept: and refuse the
	               ones we never asked for, rather than inventing a transition.
	state     -> - TestState_ShouldMapEveryReportedState
	accepts   -> - TestAccepts_ShouldMapPreShutdownAndNeverShutdown: the bit that
	               decides whether a reboot truncates the drain.
	status    -> - TestStatus_ShouldRenderWaitHintAsMilliseconds: the SCM reads
	               the hint in ms, and a Duration passed raw is off by 10^6.
	waitHintMillis -> - TestWaitHintMillis_ShouldClampRatherThanWrap: a wrapped
	                    hint tells the SCM to stop waiting almost immediately.

Tested elsewhere:

	The sequencing this file translates: controller_test.go, which runs on every
	platform and is where the exit-code rule, the checkpoint ticker and the
	drain responsiveness are actually asserted.

	That the translation reaches a real SCM correctly: task service-gate, and
	the M4a Phase -1 measurements that produced these mappings in the first
	place -- sc.exe query reported NOT_STOPPABLE while pending and
	"STOPPABLE, ACCEPTS_PRESHUTDOWN" while running, which is accepts() and
	the controller's report order observed from outside.

Declined:

	Driving svc.Run itself. It calls StartServiceCtrlDispatcher, which fails
	outside a real service host, so a test could only assert the error -- and
	the interesting behaviour is on the other side of that call. The gate covers
	it.

	Testing Execute's request pump against a fake SCM. The pump's contract is
	"do not buffer, do not drop, stop on done", and the property that matters --
	the controller staying responsive to an unbuffered channel during the drain
	-- is asserted in controller_test.go against exactly that shape.

Additional Remarks:

	These tests run only on the WS2022 gate (task test:windows), which is the
	honest place for them: everything here is a mapping onto x/sys types that do
	not exist elsewhere. That is also why this file is as thin as it is -- the
	logic worth iterating on lives in controller.go, where task ci reaches it.
*/
package winsvc

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows/svc"
)

func TestTranslate_ShouldMapTheControlsWeAccept(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in        svc.Cmd
		want      Command
		wantKnown bool
	}{
		"should map interrogate":       {in: svc.Interrogate, want: CmdInterrogate, wantKnown: true},
		"should map stop":              {in: svc.Stop, want: CmdStop, wantKnown: true},
		"should map pre-shutdown":      {in: svc.PreShutdown, want: CmdPreShutdown, wantKnown: true},
		"should refuse pause":          {in: svc.Pause, wantKnown: false},
		"should refuse continue":       {in: svc.Continue, wantKnown: false},
		"should refuse shutdown":       {in: svc.Shutdown, wantKnown: false},
		"should refuse a param change": {in: svc.ParamChange, wantKnown: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			got, known := translate(tc.in)

			// ASSERT
			// Shutdown is refused deliberately: we accept pre-shutdown instead,
			// so the SCM should never send it, and acting on one would report a
			// transition the controller did not make.
			assert.Equal(t, tc.wantKnown, known)

			if tc.wantKnown {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestState_ShouldMapEveryReportedState(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   State
		want svc.State
	}{
		"should map start pending": {in: StateStartPending, want: svc.StartPending},
		"should map running":       {in: StateRunning, want: svc.Running},
		"should map stop pending":  {in: StateStopPending, want: svc.StopPending},
		"should map stopped":       {in: StateStopped, want: svc.Stopped},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.Equal(t, tc.want, state(tc.in))
		})
	}
}

func TestAccepts_ShouldMapPreShutdownAndNeverShutdown(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	running := accepts(AcceptStop | AcceptPreShutdown)
	pending := accepts(0)

	// ASSERT
	// A Running that accepts nothing makes sc stop fail with 1052; a Running
	// that accepts Shutdown gets its drain truncated to WaitToKillServiceTimeout,
	// measured at 5000ms on this platform.
	assert.Equal(t, svc.AcceptStop|svc.AcceptPreShutdown, running)
	assert.Zero(t, pending&svc.AcceptShutdown, "shutdown must never be accepted")
	assert.Zero(t, running&svc.AcceptShutdown, "shutdown must never be accepted")
	assert.Equal(t, svc.Accepted(0), pending)
}

func TestStatus_ShouldRenderWaitHintAsMilliseconds(t *testing.T) {
	t.Parallel()

	// ARRANGE
	rep := Report{
		State:      StateStopPending,
		Accepts:    0,
		CheckPoint: 7,
		WaitHint:   20 * time.Second,
	}

	// ACT
	got := status(rep)

	// ASSERT
	// The SCM reads WaitHint in milliseconds. A time.Duration handed over raw
	// is nanoseconds, so a 20s hint would arrive as 20 billion and overflow the
	// uint32 into something meaningless.
	assert.Equal(t, uint32(20000), got.WaitHint)
	assert.Equal(t, uint32(7), got.CheckPoint)
	assert.Equal(t, svc.StopPending, got.State)
}

func TestWaitHintMillis_ShouldClampRatherThanWrap(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   time.Duration
		want uint32
	}{
		"should render an ordinary hint in milliseconds": {in: 20 * time.Second, want: 20000},
		"should floor a negative duration at zero":       {in: -time.Second, want: 0},
		"should floor a zero duration at zero":           {in: 0, want: 0},
		"should clamp a duration beyond uint32 ms":       {in: 100 * 24 * time.Hour, want: math.MaxUint32},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			// Wrapping instead of clamping would turn a long hint into a short
			// one and have the SCM kill a drain that was proceeding correctly.
			assert.Equal(t, tc.want, waitHintMillis(tc.in))
		})
	}
}
