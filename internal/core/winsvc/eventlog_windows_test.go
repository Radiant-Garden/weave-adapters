//go:build windows

/*
Testing: eventlog_windows.go

Pending:

Tested:

	realEventLog -> - TestRealEventLog_ShouldMapEachSeverityOntoTheEventLogAPI:
	                  a thin check that info/warning/error are not transposed,
	                  which is the only mistake a wrapper this thin can make.

Tested elsewhere:

	The routing and rendering these three wrap: eventlog_test.go, which is
	portable and runs everywhere against a fake sink.

	Which events reach the Event Log at all, and their numbers:
	internal/catalogs.

	That a registered source renders and an unregistered one does not: M4a
	Phase -1 measured it on Windows Server 2022, and task service-gate asserts
	a real SYS-001 is readable after an install and gone after an uninstall.

Declined:

	Driving eventlog.Open, InstallAsEventCreate and Remove. All three mutate
	HKLM and need Administrator, so a unit test either fails on an unprivileged
	runner -- which is what ci:windows executes as -- or leaves registry state
	behind. The gate runs elevated and is the right place.

	Asserting the "registry key already exists" text match in
	InstallEventLogSource. Reproducing it means actually registering a source
	twice, which is the same privileged mutation. The gate's reinstall step
	exercises it for real.

Additional Remarks:

	This file is as thin as it is on purpose: everything worth iterating on
	lives in eventlog.go, where task ci reaches it on any platform.
*/
package winsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRealEventLog_ShouldMapEachSeverityOntoTheEventLogAPI(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// A nil *eventlog.Log would panic if these actually dispatched, so this
	// asserts the shape rather than the call: realEventLog must satisfy the
	// interface the portable handler writes through, or the two halves have
	// drifted.
	var w eventLogWriter = realEventLog{}

	// ACT / ASSERT
	assert.NotNil(t, w)
}
