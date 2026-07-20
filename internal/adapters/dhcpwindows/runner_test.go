/*
Testing: runner.go

Pending:

	Verifying on a real WS2022 host that -NoProfile -NonInteractive with the Stop
	  preference leaves stderr silent across a *successful* run. That is what
	  would let stderr's presence become a typed error rather than context on
	  other errors, and it cannot be established from darwin.

Tested:

	execRunner.run
	  - TestExecRunner_ShouldReturnStdoutAndStderrSeparately: the two streams do not
	    contaminate each other, which is what lets stdout be decoded as pure JSON.
	  - TestExecRunner_ShouldReportANonZeroExit: an exit code reaches the caller as
	    an error rather than as empty output.
	  - TestExecRunner_ShouldPassTheServerThroughTheEnvironment: the injection-free
	    parameter path, asserted end to end against a real child process.
	  - TestExecRunner_ShouldReturnATimeoutWhenTheContextExpires: the deadline is
	    classified as a timeout, not as a generic shell fault.
	  - TestExecRunner_ShouldNotBlockPastTheDeadlineOnAWedgedChild: the WaitDelay
	    guarantee — the one that makes the probe timeout mean anything.
	  - TestExecRunner_ShouldFailWhenTheShellIsMissing: an absent powershell.exe is
	    an error, which is what the health probe exists to surface.

Tested elsewhere:

	Classification of these failures into the typed backend errors: client_test.go.

Declined:

	That a *successful* run is never reclassified as a timeout. runArgs examines
	  ctx.Err() only when Run returned an error, so a clean run whose deadline
	  passes a moment later keeps its output — but the window is the few
	  instructions between those two statements, and no test can land inside it
	  without a clock seam in production code. Injecting one to observe a
	  nanosecond race would be more machinery than the bug is worth, so the
	  behaviour is argued in a comment at the site instead of asserted here.

	Running actual powershell.exe: CI is darwin and the point of the runner seam is
	  that the package needs no Windows host and no build tags. These tests drive
	  /bin/sh instead, which exercises the same exec.Cmd plumbing — process
	  spawning, stream separation, environment passing, context kill and WaitDelay
	  — since none of that is shell-specific. What *is* PowerShell-specific is the
	  script text, asserted in scope_test.go, and the output shapes, replayed from
	  captured fixtures in client_test.go.

Additional Remarks:

	The wedged-child test is the reason WaitDelay exists, so it is written to fail
	  if WaitDelay is removed: a grandchild holds the inherited stdout pipe open
	  after its parent is killed, which is precisely the case where Wait blocks
	  past the context deadline forever. Without WaitDelay it hangs rather than
	  failing, so it carries its own bound and reports the hang as a failure.
*/
package dhcpwindows

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireUnixShell skips on Windows, where /bin/sh does not exist. The
// production path there is powershell.exe, which these tests do not drive.
func requireUnixShell(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("drives /bin/sh to exercise the exec plumbing; not present on Windows")
	}
}

// sh runs a script through execRunner's own runArgs, so what is under test is
// the production exec configuration — WaitDelay included — with only the shell
// and its argument list swapped. A test that assembled its own exec.Cmd would
// keep passing if WaitDelay were deleted from runner.go.
func sh(ctx context.Context, t *testing.T, server, script string, env map[string]string) ([]byte, []byte, error) {
	t.Helper()

	return execRunner{path: "/bin/sh", server: server}.runArgs(ctx, env, "-c", script)
}

func TestExecRunner_ShouldReturnStdoutAndStderrSeparately(t *testing.T) {
	t.Parallel()
	requireUnixShell(t)

	// ARRANGE / ACT
	stdout, stderr, err := sh(context.Background(), t, "", `echo '[]'; echo 'a warning' >&2`, nil)

	// ASSERT — stdout has to stay pure JSON: anything the shell says on stderr
	// leaking into it would break the decode, which is the failure mode
	// -NoProfile also guards against.
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(string(stdout)))
	assert.Contains(t, string(stderr), "a warning")
}

func TestExecRunner_ShouldReportANonZeroExit(t *testing.T) {
	t.Parallel()
	requireUnixShell(t)

	// ARRANGE / ACT — what $ErrorActionPreference = 'Stop' turns a permissions
	// failure into, instead of an empty pipeline and a zero exit.
	_, stderr, err := sh(context.Background(), t, "", `echo 'Access is denied.' >&2; exit 5`, nil)

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, string(stderr), "Access is denied")
}

func TestExecRunner_ShouldPassTheServerThroughTheEnvironment(t *testing.T) {
	t.Parallel()
	requireUnixShell(t)

	// ARRANGE — the value that must never appear in script text.
	const server = "dhcp01.example.test"

	// ACT — the script reads it from the environment, exactly as
	// listScopesScript does with $env:WADAPT_DHCP_SERVER.
	stdout, _, err := sh(context.Background(), t, server, `printf '%s' "$`+envServerName+`"`, nil)

	// ASSERT — this is the whole injection-free parameter path, end to end
	// against a real child process rather than asserted on a string.
	require.NoError(t, err)
	assert.Equal(t, server, string(stdout))
}

func TestExecRunner_ShouldReturnATimeoutWhenTheContextExpires(t *testing.T) {
	t.Parallel()
	requireUnixShell(t)

	// ARRANGE
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// ACT
	_, _, err := sh(ctx, t, "", "sleep 10", nil)

	// ASSERT — a timeout and a shell fault have different fixes (raise
	// dhcp.commandTimeout versus repair the host), so they must not arrive as
	// one error.
	require.ErrorIs(t, err, ErrBackendTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestExecRunner_ShouldNotBlockPastTheDeadlineOnAWedgedChild(t *testing.T) {
	t.Parallel()
	requireUnixShell(t)

	// ARRANGE — a grandchild that outlives its parent while holding the
	// inherited stdout pipe open. This is the case where exec.CommandContext
	// kills the process but Wait blocks on the pipe indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	// ACT
	go func() {
		_, _, err := sh(ctx, t, "", "sleep 30 & sleep 30", nil)
		done <- err
	}()

	// ASSERT — WaitDelay is what bounds this. Without it the call hangs rather
	// than failing, so the test carries its own bound and reports the hang.
	// The health probe's shorter timeout only delivers on its promise because
	// of this: health.refresh holds its mutex across the probe, so an unbounded
	// Wait would serialize every health poll behind one wedged shell.
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrBackendTimeout)
	case <-time.After(RunnerKillGrace + 5*time.Second):
		t.Fatal("run did not return after the context expired: WaitDelay is not bounding Wait")
	}
}

func TestExecRunner_ShouldFailWhenTheShellIsMissing(t *testing.T) {
	t.Parallel()

	// ARRANGE — the RSAT-DHCP-absent shape: the binary simply is not there.
	runner := execRunner{path: filepathJoinNonexistent(t)}

	// ACT
	_, _, err := runner.run(context.Background(), "whatever", nil)

	// ASSERT — an error rather than empty output, so the health probe reports
	// unhealthy instead of the endpoint 500ing on every request.
	require.Error(t, err)
}

// filepathJoinNonexistent returns a path guaranteed not to exist.
func filepathJoinNonexistent(t *testing.T) string {
	t.Helper()

	return t.TempDir() + string(os.PathSeparator) + "no-such-shell"
}
