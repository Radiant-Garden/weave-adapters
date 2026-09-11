//go:build !windows

/*
Testing: eventlog_other.go

Pending:

Tested:

	NewEventLogHandler / InstallEventLogSource / RemoveEventLogSource
	  - TestEventLogStubs_ShouldRefuseOffWindows: all three, because a stub that
	    quietly succeeded would let a macOS build believe it had an Event Log.

Tested elsewhere:

	The handler these stand in for: eventlog_test.go, which is portable and
	runs here.

	The real implementations: eventlog_windows.go, and task service-gate
	against a real registry.

Declined:

	Nothing.

Additional Remarks:

	Discarding rather than erroring was the tempting alternative and is worse:
	the binary reaches these only after IsService reported true, which cannot
	happen here, so silence would turn a wiring mistake into a mystery.
*/
package winsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventLogStubs_ShouldRefuseOffWindows(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	handler, closer, err := NewEventLogHandler("wadapt-test")

	// ASSERT
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Nil(t, handler)
	assert.Nil(t, closer)

	require.ErrorIs(t, InstallEventLogSource("wadapt-test"), ErrUnsupported)
	require.ErrorIs(t, RemoveEventLogSource("wadapt-test"), ErrUnsupported)
}
