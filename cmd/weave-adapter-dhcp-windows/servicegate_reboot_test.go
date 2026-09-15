//go:build servicegate && windows

/*
Testing: the pre-shutdown drain across a real reboot (no corresponding .go
file)

Pending:

Tested:

	TestServiceGateReboot_Before -> confirms the provisioned service is
	  actually serving, touches it once so it has served before the host goes
	  down, and records the moment. Run this, then reboot.
	TestServiceGateReboot_After  -> asserts the drain COMPLETED before the
	  reboot, and that the service came back and serves without a login.

Tested elsewhere:

	A drain with a REQUEST GENUINELY IN FLIGHT: servicegate_test.go steps 14
	and 15, using the slow backend stub. That is where the drain is observed
	doing work.

Declined -- and this is the honest limit of this file:

	Arming the slow stub before the reboot. What these two halves prove is
	that the PRE-SHUTDOWN PATH yields a clean SYS-004 and the service comes
	back: the service is asked to stop by the SCM's shutdown machinery rather
	than by a Stop control, and it reports a clean shutdown rather than being
	killed. What they do NOT prove is that a LONG drain survives the
	pre-shutdown wall -- the provisioned service answers a scopes request in
	seconds, so little is in flight by the time the host goes down.

	Arming it properly would mean reconfiguring the PRODUCTION service to use
	a sleeping backend, rebooting the host in that state, and relying on the
	after-half to put it back. A production DHCP adapter left pointing at a
	stub because the second half never ran is a worse outcome than the gap,
	so the gap is stated instead of closed.

Declined:

	Rebooting from the test. Nothing in a test binary survives one, and a gate
	that rebooted a DHCP server as a side effect of `go test` would be a worse
	idea than the bug it is looking for.

Additional Remarks:

	TWO HALVES, JOINED BY A MARKER FILE, so only the reboot itself is manual:

	    task service-gate:reboot-before
	    <reboot the host>
	    task service-gate:reboot-after

	Written as a prose checklist this would be performed once, at sign-off,
	and never again. Split this way the assertions survive as something a
	later change can be re-checked against -- which matters, because this is
	the only place the pre-shutdown wall is ever observed.

	The marker records the pre-reboot timestamp. The after-half asserts
	SYS-004 appears BEFORE it: a SYS-004 from the restart would otherwise
	satisfy a naive "did it shut down cleanly" check while proving nothing
	about the reboot that happened in between.
*/
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rebootMarker is where the before-half leaves what the after-half needs.
// Under ProgramData rather than a temp directory, because a reboot may clear
// the latter.
var rebootMarker = filepath.Join(os.Getenv("ProgramData"), "weave-adapters", "gate-reboot-marker.json")

// rebootState is what survives the reboot.
type rebootState struct {
	// ArmedAt is the moment the before-half finished. Every assertion in the
	// after-half is relative to it.
	ArmedAt time.Time `json:"armedAt"`
	// BaseURL is where the service was serving, so the after-half can check
	// it came back.
	BaseURL string `json:"baseUrl"`
	// LogFile is the service's log, which persists across the reboot and is
	// where the drain's outcome is recorded.
	LogFile string `json:"logFile"`
}

//nolint:paralleltest // drives a real service on one host
func TestServiceGateReboot_Before(t *testing.T) {
	requireElevated(t)

	provisioned := os.Getenv(provisionedConfigEnv)
	require.NotEmpty(t, provisioned, "%s must name the production config", provisionedConfigEnv)

	require.Equal(t, "RUNNING", state(t),
		"the provisioned service must be installed and running before arming the reboot check")

	logFile := configuredLogFile(t, provisioned)
	baseURL := configuredBaseURL(t, provisioned)

	// IN-PROCESS, and never a detached Start-Process. The detached version of
	// this ended every run -- passing or failing -- in `Test I/O incomplete
	// 1m0s after exiting`: the grandchild outlived the test binary still
	// holding the stream `go test` reads its output through, so `go test`
	// waited out its WaitDelay and reported that instead of the result. A
	// gate that cannot report its own verdict is worse than no gate, and this
	// one costs a reboot to re-run.
	//
	// Nothing needed detaching. This is a WARM-UP -- the service should have
	// served once before the host goes down -- not a request that has to be
	// in flight at the reboot, which the Declined section above is explicit
	// about not attempting. A synchronous GET is the whole requirement, and
	// it doubles as the precondition: arming against a service that is
	// registered but not answering would record a marker that proves nothing.
	//
	// It also drops a PowerShell string that was wrong in two places -- `''`
	// around the URI and a cmd.exe `^|` inside a PowerShell argument -- which
	// no longer has anywhere to hide now that the request is Go's.
	t.Logf("touching %s so the service has served at least once before the reboot", baseURL)
	waitReady(t, baseURL+"/api/v1/health")

	state := rebootState{ArmedAt: time.Now(), BaseURL: baseURL, LogFile: logFile}

	body, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(rebootMarker), 0o700))
	require.NoError(t, os.WriteFile(rebootMarker, body, 0o600))

	t.Logf("ARMED at %s. Now reboot the host, then run: task service-gate:reboot-after",
		state.ArmedAt.Format(time.RFC3339))
}

//nolint:paralleltest // drives a real service on one host
func TestServiceGateReboot_After(t *testing.T) {
	requireElevated(t)

	//nolint:gosec // G304: a marker this gate wrote.
	body, err := os.ReadFile(rebootMarker)
	require.NoError(t, err, "no marker at %s: run task service-gate:reboot-before first", rebootMarker)

	var armed rebootState

	require.NoError(t, json.Unmarshal(body, &armed))

	// Removed ONLY on success. A failed run that deleted the marker would
	// destroy the one piece of state that cannot be recreated without another
	// reboot of a production DHCP server -- so the first failure would cost a
	// second outage just to see it again.
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.Remove(rebootMarker)
		} else {
			t.Logf("marker kept at %s so this can be re-run without another reboot", rebootMarker)
		}
	})

	t.Logf("armed at %s; reading the boot time", armed.ArmedAt.Format(time.RFC3339))

	bootedAt := lastBootTime(t)

	t.Logf("host booted at %s", bootedAt.Format(time.RFC3339))
	require.True(t, bootedAt.After(armed.ArmedAt),
		"the host has not rebooted since the marker was written (armed %s, booted %s)",
		armed.ArmedAt.Format(time.RFC3339), bootedAt.Format(time.RFC3339))

	// The assertion this whole split exists for. SYS-004 means the drain
	// finished; SYS-007 means the SCM's wall was tighter than the drain, and
	// nothing at all means it was killed outright.
	//
	// Bounded to the window between arming and the reboot, or a SYS-004 from
	// the service starting and stopping again afterwards would satisfy it
	// while proving nothing.
	t.Logf("reading the service log at %s", armed.LogFile)

	logBody, err := os.ReadFile(armed.LogFile)
	require.NoError(t, err, "the service's log did not survive the reboot")

	drained := lastEventBetween(t, string(logBody), "SYS-004", armed.ArmedAt, bootedAt)
	truncated := lastEventBetween(t, string(logBody), "SYS-007", armed.ArmedAt, bootedAt)

	assert.False(t, truncated,
		"SYS-007 before the reboot: the pre-shutdown deadline did not cover the drain")
	assert.True(t, drained,
		"no SYS-004 between arming and the reboot: the drain did not complete, so the "+
			"service was cut off rather than shut down")

	// And it came back, with nobody logged in.
	t.Log("waiting for the service to report running")
	waitState(t, "RUNNING", 3*time.Minute)
	waitReady(t, armed.BaseURL+"/api/v1/health")
}
