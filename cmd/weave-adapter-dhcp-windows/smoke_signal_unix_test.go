//go:build (smoke || e2e) && !windows

/*
Testing: interrupt delivery off Windows

Pending:

Tested:

Tested elsewhere:
  Both helpers are exercised by TestSmoke_ShouldServeHealthAndEnforceAuthWhenRunAsABinary
  in smoke_test.go, which is the only caller.

Declined:
  A unit test for Process.Signal. Asserting that the standard library delivers
  a signal tests the standard library.

Additional Remarks:
  The Windows build of this pair needs a console process group and the console
  control API; here a signal is just a signal, so newProcessGroup has nothing
  to do. The split exists so the smoke test itself stays platform-agnostic.
*/

package main

import (
	"os"
	"os/exec"
)

// newProcessGroup is a no-op off Windows: os.Interrupt reaches the child
// directly, without needing it isolated from the test process first.
func newProcessGroup(_ *exec.Cmd) {}

// interrupt asks the adapter to shut down the way Ctrl+C does.
func interrupt(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}
