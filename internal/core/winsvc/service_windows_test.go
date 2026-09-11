//go:build windows

/*
Testing: service_windows.go

Pending:

Tested:

	fromSvcState  -> - TestFromSvcState_ShouldMapEveryStateTheSCMReports
	startTypeName -> - TestStartTypeName_ShouldDistinguishDelayedAutoStart: the
	                   one distinction an operator checking a boot problem needs.
	serviceKeyPath -> - TestServiceKeyPath_ShouldAddressTheServicesHive

Tested elsewhere:

	Install, Uninstall, Start, Stop and Status against a real Service Control
	Manager: task service-gate. The behaviours they encode -- the
	failure-actions flag, the settable PreshutdownTimeout, the unquoted
	ImagePath, the stop-before-delete handle discipline -- were measured on
	Windows Server 2022 in M4a Phase -1 before this code was written.

	What the subcommand asks for: cmd/weave-adapter-dhcp-windows/service_test.go,
	against a fake Manager, on every platform.

Declined:

	Unit-testing the Manager implementation. Every method either connects to
	the SCM or writes under HKLM, both of which need Administrator. A test
	that did so would fail on ci:windows -- which runs as an unprivileged
	service account by design -- or leave a registered service behind on the
	one host the milestone depends on. The elevated gate is the right place,
	and it asserts the observable results rather than the calls.

	Testing transitionTimeout by waiting it out. Four minutes of a test suite
	to observe a constant.

Additional Remarks:

	Only the pure helpers are tested here, and that is the honest boundary:
	everything else in this file is a sequence of privileged calls whose
	interesting behaviour is the SCM's, not ours.
*/
package winsvc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestFromSvcState_ShouldMapEveryStateTheSCMReports(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   svc.State
		want State
	}{
		"should map start pending": {in: svc.StartPending, want: StateStartPending},
		"should map running":       {in: svc.Running, want: StateRunning},
		"should map stop pending":  {in: svc.StopPending, want: StateStopPending},
		"should map stopped":       {in: svc.Stopped, want: StateStopped},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.Equal(t, tc.want, fromSvcState(tc.in))
		})
	}
}

func TestStartTypeName_ShouldDistinguishDelayedAutoStart(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	// The distinction an operator needs when the adapter answered 503 for the
	// first two minutes after a reboot: delayed or not is the whole question.
	assert.Equal(t, "automatic", startTypeName(mgr.StartAutomatic, false))
	assert.Equal(t, "automatic (delayed)", startTypeName(mgr.StartAutomatic, true))
	assert.Equal(t, "manual", startTypeName(mgr.StartManual, false))
	assert.Equal(t, "disabled", startTypeName(mgr.StartDisabled, false))
}

func TestServiceKeyPath_ShouldAddressTheServicesHive(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	got := serviceKeyPath("wadapt-dhcp-windows")

	// ASSERT
	assert.True(t, strings.HasSuffix(got, `\wadapt-dhcp-windows`))
	assert.Contains(t, got, `CurrentControlSet\Services`)
}
