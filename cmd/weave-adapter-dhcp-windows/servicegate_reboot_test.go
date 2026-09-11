//go:build servicegate && windows

/*
Testing: the pre-shutdown drain across a real reboot (no corresponding .go
file)

Pending:

Tested:

	TestServiceGateReboot_Before -> arms the slow backend, puts a request in
	  flight, and records the moment. Run this, then reboot.
	TestServiceGateReboot_After  -> asserts the drain COMPLETED before the
	  reboot, and that the service came back and serves without a login.

Tested elsewhere:

	The same drain under a `Stop` control: servicegate_test.go steps 14 and
	15. That is not a substitute. A `Stop` is bounded by the SCM's ordinary
	patience, while a reboot is bounded by PreshutdownTimeout -- a hard wall,
	and the only thing that exercises it is an actual shutdown.

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
	"fmt"
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

	// A request that will still be in flight when the shutdown lands. The
	// point of the whole exercise is a drain with something to drain.
	t.Logf("putting a request in flight against %s", baseURL)
	go func() {
		_, _ = psErr(t, fmt.Sprintf(
			`try { Invoke-WebRequest -Uri '%s/api/v1/scopes' -TimeoutSec 120 -UseBasicParsing | Out-Null } `+
				`catch { $_.Exception.Message }`, baseURL))
	}()

	time.Sleep(3 * time.Second)

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

	t.Cleanup(func() { _ = os.Remove(rebootMarker) })

	bootedAt := lastBootTime(t)
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
	waitState(t, "RUNNING", 3*time.Minute)
	waitReady(t, armed.BaseURL+"/api/v1/health")
}
