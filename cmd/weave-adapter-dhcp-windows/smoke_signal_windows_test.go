//go:build (smoke || e2e) && windows

/*
Testing: interrupt delivery on Windows

Pending:

Tested:

Tested elsewhere:
  Both helpers are exercised by TestSmoke_ShouldServeHealthAndEnforceAuthWhenRunAsABinary
  in smoke_test.go, which is the only caller. They have no behaviour worth
  asserting apart from the process actually stopping.

Declined:
  A unit test for GenerateConsoleCtrlEvent. It has no observable result short of
  signalling a real process, which is what the smoke test already does.

Additional Remarks:
  Windows has no kill(2). os.Process.Signal rejects os.Interrupt outright, so
  the console control API is the only way to ask a console exe to stop the way
  an operator's Ctrl+C does -- which is exactly the path main.go assumes and
  nothing had verified.

  CTRL_BREAK rather than CTRL_C: CREATE_NEW_PROCESS_GROUP disables CTRL_C
  handling for the new group, and Go maps both events to os.Interrupt anyway.
  The new group is what keeps the event off the test process, which shares this
  console and would otherwise take itself down with it.
*/

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGenerateConsoleCtrlEvnt = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

const ctrlBreakEvent = 1

// newProcessGroup makes the child the root of its own console process group, so
// an event sent to it does not travel to the test process.
func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// interrupt asks the adapter to shut down the way Ctrl+C does.
func interrupt(cmd *exec.Cmd) error {
	result, _, err := procGenerateConsoleCtrlEvnt.Call(
		uintptr(ctrlBreakEvent),
		uintptr(cmd.Process.Pid),
	)
	if result == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent to pid %d: %w", cmd.Process.Pid, err)
	}

	return nil
}
