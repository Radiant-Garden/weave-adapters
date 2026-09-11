//go:build !windows

/*
Testing: winsvc_other.go

Pending:

Tested:

	IsService -> - TestIsService_ShouldReportFalseOffWindows: the console path is
	               selected by this answer, not by a build tag at the call site.
	Run       -> - TestRun_ShouldRefuseOffWindowsRatherThanServeDirectly: a
	               mistaken call fails loudly instead of looking like it worked.

Tested elsewhere:

	The sequencing these two stub out: controller_test.go, which runs here too.

	The real implementations: winsvc_windows.go, compiled by build-windows and
	exercised by task test:windows and task service-gate.

Declined:

	Nothing. The file is two functions and both are asserted.

Additional Remarks:

	This file exists so the binary's wiring compiles and runs unchanged on a
	developer machine. The value of testing it is small but the cost is smaller,
	and a stub that silently returned the wrong answer would send a macOS build
	down the service path with no way to notice.
*/
package winsvc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsService_ShouldReportFalseOffWindows(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	is, err := IsService()

	// ASSERT
	require.NoError(t, err)
	assert.False(t, is)
}

func TestRun_ShouldRefuseOffWindowsRatherThanServeDirectly(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var served bool

	serve := func(context.Context, func()) error {
		served = true

		return nil
	}

	// ACT
	err := Run("wadapt-test", time.Second, serve)

	// ASSERT
	// Falling back to serving would make a mistaken call look like it worked.
	require.ErrorIs(t, err, ErrUnsupported)
	assert.False(t, served, "Run must not fall back to serving off Windows")
}
