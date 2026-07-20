package dhcpwindows

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// envServerName is the child-process variable carrying the -ComputerName
// target. Values reach a script this way rather than by string interpolation;
// see listScopesScript.
const envServerName = "WADAPT_DHCP_SERVER"

// envPrefix namespaces every variable a script reads, so a value this package
// passes cannot be confused with one the host environment already had.
const envPrefix = "WADAPT_"

// RunnerKillGrace bounds how long Wait may block after the context kills the
// process.
//
// This is load bearing, not a nicety. exec.CommandContext kills the process on
// context expiry, but Wait still blocks until the stdout pipe closes, and an
// inherited handle in a wedged child keeps it open indefinitely — so without
// WaitDelay the timeout does not actually bound the call. The health probe's
// separate, shorter timeout only delivers on its promise if Wait is bounded
// too, since health.refresh holds its mutex across the probe and a hung
// powershell.exe would serialize every health poll behind it.
//
// Exported because the binary's HTTP write timeout must clear a full backend
// call, which is dhcp.commandTimeout plus this grace: a WriteTimeout below that
// sum would truncate a slow-but-honest response.
const RunnerKillGrace = 2 * time.Second

// runner turns a PowerShell script into its stdout bytes.
//
// The client takes one rather than calling exec.Command itself, which is what
// keeps the whole package OS-agnostic Go that happens to shell out: the tests
// run on darwin against a fake, and no build tags are needed anywhere.
type runner interface {
	// env carries the script's parameters. This is the ONLY way a runtime value
	// reaches a script: nothing is interpolated into the script text, ever.
	//
	// That is not stylistic. powershell.exe -Command takes one string, so a
	// value spliced into it is code — a scope description containing
	// '; Remove-DhcpServerv4Scope -ScopeId 10.0.0.0 ;# would execute. And the
	// obvious fix does not exist here: -Command has no -ArgumentList (that
	// belongs to Invoke-Command/Start-Process/Start-Job), and a real param()
	// block needs -File, which would make execution policy relevant again.
	// The child environment has none of those problems: no quoting, no parsing,
	// no temp file.
	run(ctx context.Context, script string, env map[string]string) (stdout, stderr []byte, err error)
}

// execRunner runs scripts through a real powershell.exe.
type execRunner struct {
	// path is the shell binary. Configurable so an operator can point at pwsh
	// or a non-default location without a code change.
	path string
	// server is the -ComputerName target, empty for the local host. It is
	// passed to the child through the environment, never into the script text.
	server string
}

// run executes one script and returns its output streams.
//
// -NoProfile matters: a profile script on the host would otherwise write to
// stdout and corrupt the JSON. -NonInteractive stops the shell prompting into a
// pipe nobody is reading. Execution policy is not a concern — it governs script
// *files*, not an inline -Command.
func (r execRunner) run(ctx context.Context, script string, env map[string]string) ([]byte, []byte, error) {
	return r.runArgs(ctx, env, "-NoProfile", "-NonInteractive", "-Command", script)
}

// runArgs is run's plumbing, split from its argument list so the tests can
// drive a POSIX shell through the *production* exec configuration rather than
// a copy of it. That distinction is the point: a test that built its own
// exec.Cmd would keep passing if WaitDelay were removed from here, which is
// exactly the regression it exists to catch.
func (r execRunner) runArgs(ctx context.Context, env map[string]string, args ...string) ([]byte, []byte, error) {
	// G204: launching a subprocess is this package's entire purpose, so the
	// finding cannot be designed away — but neither input is attacker-reachable.
	// r.path is operator-provisioned configuration (dhcp.powershellPath), the
	// same trust level as the binary's own path. args are compile-time
	// constants plus a script constant: nothing derived from a request, and no
	// value interpolated into the script text. The one runtime value any script
	// needs travels in the child's environment instead, which is what keeps
	// that true — see listScopesScript.
	//nolint:gosec // G204: constant args; path is provisioned config, not input.
	cmd := exec.CommandContext(ctx, r.path, args...)

	// See RunnerKillGrace: without this the context deadline does not bound the
	// call, only the process.
	cmd.WaitDelay = RunnerKillGrace

	// The child inherits the parent environment and adds the target to it.
	// Inheriting is deliberate rather than lax: powershell.exe needs PATH,
	// SystemRoot and PSModulePath to locate itself and the DhcpServer module at
	// all, so a curated environment would have to reconstruct most of one and
	// would break on the first host that keeps its modules somewhere unusual.
	// What matters for injection is the other direction — that the target
	// travels here instead of being built into the command text.
	cmd.Env = append(cmd.Environ(), envServerName+"="+r.server)

	// Script parameters, each under the WADAPT_ prefix. A caller cannot reach
	// anything else in the child's environment: a key outside the prefix is
	// dropped rather than set, so a bug upstream cannot overwrite PATH or
	// PSModulePath and change which module the shell loads.
	for key, value := range env {
		if !strings.HasPrefix(key, envPrefix) {
			continue
		}

		cmd.Env = append(cmd.Env, key+"="+value)
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	// Only a *failed* run is examined for a deadline. A run that returned
	// cleanly produced complete output, and reclassifying it because the clock
	// happened to pass the deadline between Run returning and this line would
	// discard a good answer — a race that would surface as a rare, unreproducible
	// timeout on a healthy host. A process killed by the context always reports
	// an error, so nothing real is missed by gating on one.
	if err != nil {
		// Only an expired *deadline* is a timeout. ctx.Err() is also non-nil for
		// context.Canceled, which is what a graceful shutdown or a disconnected
		// client produces — telling an operator draining the server that "the
		// dhcp backend timed out" would send them to raise dhcp.commandTimeout
		// for a problem that was a shutdown.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stdout.Bytes(), stderr.Bytes(), errors.Join(ErrBackendTimeout, ctx.Err())
		}

		// A cancellation is classified here rather than left to fall through.
		// "Reported as what it is" was the intent, but what it fell through to
		// was the *exec.ExitError from the context-kill, which runError reads as
		// a non-zero exit and classifies as ErrBackendUnavailable — so every
		// mid-request disconnect and every graceful drain logged BACKEND-101 at
		// ERROR saying "powershell exited -1". That is a false outage signal on
		// the one occasion an operator is most likely to be reading the log.
		if errors.Is(ctx.Err(), context.Canceled) {
			return stdout.Bytes(), stderr.Bytes(), errors.Join(ErrBackendCanceled, ctx.Err())
		}

		return stdout.Bytes(), stderr.Bytes(), err
	}

	return stdout.Bytes(), stderr.Bytes(), nil
}
