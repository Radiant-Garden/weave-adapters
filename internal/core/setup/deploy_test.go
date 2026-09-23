/*
Testing: deploy.go

Pending:

Tested:
  binaryStep
    - TestBinaryStep_ShouldCopyTheBinaryToTheServiceDirectory: and register the destination, not the source.
    - TestBinaryStep_ShouldProjectTheDestinationDuringCheck: the install step's Check needs it before anything is copied.
    - TestBinaryStep_ShouldLeaveAnIdenticalBinaryAlone: compared by hash, not by timestamp.
    - TestBinaryStep_ShouldBlockAnUpgradeWhileTheServiceHoldsTheBinaryOpen
    - TestBinaryStep_ShouldReplaceTheBinaryWhenRestartWasAskedFor: and stop the service first.
    - TestBinaryStep_ShouldRegisterTheBinaryWhereItIsWhenCopyingIsRefused
  installStep
    - TestInstallStep_ShouldRegisterAnAbsentService
    - TestInstallStep_ShouldLeaveAMatchingRegistrationAlone: compared against Command, never the escaped BinPath.
    - TestInstallStep_ShouldBlockARegistrationThatDiffers: and name --reinstall.
    - TestInstallStep_ShouldBlockARegistrationItCannotReadBack: cannot-confirm is not confirmed.
    - TestInstallStep_ShouldReplaceARegistrationWhenReinstallWasAskedFor
  startStep
    - TestStartStep_ShouldStartAStoppedService
    - TestStartStep_ShouldLeaveARunningServiceAlone
    - TestStartStep_ShouldBlockARunningServiceThatNeedsARestart: and name --restart.
    - TestStartStep_ShouldStopBeforeStartingWhenRestarting

Tested elsewhere:
  setup.Install itself — the validation, the binary-directory refusal, the
  lockdown and their ordering: install_test.go. The install STEP is only the
  drift check layered on top of it.

  That an SCM really refuses to replace a running service's image, and that a
  registration round-trips through CreateService: task service-gate.

  How ServiceStatus.Command is produced from an escaped ImagePath:
  internal/core/winsvc/service_windows.go, which has no unit coverage by
  design — the double fills the field directly, which is what lets these tests
  run anywhere.

Declined:
  Asserting the copy is byte-identical to the source beyond its hash. The hash
  IS the assertion; comparing contents as well would restate it.

Additional Remarks:
  The registration comparison is the subtle one. ServiceStatus.BinPath is the
  ImagePath with the SCM's own escaping still on it, while a Definition holds
  unescaped values — so a check that compared those two would need a portable
  re-implementation of EscapeArg, and would be wrong in exactly the cases that
  matter: a path with a space, a quote, a trailing backslash. Comparing
  Command, which Windows itself decomposes, is what makes the check correct
  without this package knowing the escaping rules.

  A nil Command is treated as "cannot confirm" rather than "differs", and
  blocked either way. Off Windows the stub leaves it nil, so a run there says
  it cannot vouch for a registration rather than silently claiming a match.
*/

package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc/winsvctest"
)

// runningStatus is what the SCM reports for a service that is installed and up
// — the only state in which the destination binary cannot simply be replaced,
// and therefore the one a test asserting on that refusal has to arrange.
func runningStatus(opts Options) winsvc.ServiceStatus {
	return winsvc.ServiceStatus{
		Installed: true,
		Name:      opts.Definition.Name,
		State:     winsvc.StateRunning,
	}
}

// writeBinary writes a stand-in executable and returns its path.
func writeBinary(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// ---------------------------------------------------------------------------
// binary
// ---------------------------------------------------------------------------

func TestBinaryStep_ShouldCopyTheBinaryToTheServiceDirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := binaryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, binaryStep{}.Apply(context.Background(), p))

	// ASSERT
	// Copying exists because install refuses a binary whose own directory an
	// unprivileged account can write — which is every Downloads folder, and
	// which for a LocalSystem service is a full escalation.
	assert.Equal(t, Pending, verdict.Condition)

	dest := filepath.Join(opts.Layout.BinDir, filepath.Base(opts.BinPath))
	assert.FileExists(t, dest)
	assert.Equal(t, dest, p.BinPath, "the source was registered rather than the copy")

	sourceSum, err := fileSum(opts.BinPath)
	require.NoError(t, err)

	destSum, err := fileSum(dest)
	require.NoError(t, err)
	assert.Equal(t, sourceSum, destSum)
}

func TestBinaryStep_ShouldProjectTheDestinationDuringCheck(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	// ACT — Check only. Nothing is copied.
	_, err := binaryStep{}.Check(context.Background(), p)

	// ASSERT
	// The install step's Check runs before any Apply, and has to compare the
	// registration against the path that WILL be registered rather than the
	// one this process happens to be running from.
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(opts.Layout.BinDir, filepath.Base(opts.BinPath)), p.BinPath)
	assert.NoFileExists(t, p.BinPath, "Check copied something")
}

func TestBinaryStep_ShouldLeaveAnIdenticalBinaryAlone(t *testing.T) {
	t.Parallel()

	// ARRANGE — the same bytes already at the destination.
	opts := runOptions(t)
	writeBinary(t, opts.Layout.BinDir, filepath.Base(opts.BinPath), "not really a binary")

	deps, _, _ := okDeps()

	// ACT
	verdict, err := binaryStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// By hash, so a re-run after a rebuild that changed nothing does not stop
	// a running service to copy the same bytes over themselves.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
}

func TestBinaryStep_ShouldBlockAnUpgradeWhileTheServiceHoldsTheBinaryOpen(t *testing.T) {
	t.Parallel()

	// ARRANGE — a DIFFERENT binary already installed, and the service RUNNING.
	// The running part has to be arranged: it is the only state in which the
	// destination cannot simply be overwritten.
	opts := runOptions(t)
	writeBinary(t, opts.Layout.BinDir, filepath.Base(opts.BinPath), "an older build")

	deps, m, _ := okDeps()
	m.Reported = runningStatus(opts)

	// ACT
	verdict, err := binaryStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// A running service holds its own executable open, so overwriting it fails
	// with a sharing violation until it is stopped. A named refusal that says
	// which flag clears it beats a copy error nobody can act on.
	require.NoError(t, err)
	assert.Equal(t, Blocked, verdict.Condition)
	assert.Contains(t, verdict.Detail, "--restart")
	assert.Contains(t, verdict.Detail, "is running")
}

func TestBinaryStep_ShouldReplaceAStaleBinaryWithoutDemandingARestart(t *testing.T) {
	t.Parallel()

	tests := map[string]winsvc.ServiceStatus{
		// What `service uninstall` leaves behind.
		"no service is registered": {Installed: false},
		// What a manual stop, or a crash, leaves behind.
		"the service is stopped": {Installed: true, State: winsvc.StateStopped},
	}

	for name, reported := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE — a different binary at the destination, nothing holding it.
			opts := runOptions(t)
			writeBinary(t, opts.Layout.BinDir, filepath.Base(opts.BinPath), "an older build")

			deps, m, _ := okDeps()
			m.Reported = reported

			p := planFor(opts, deps)

			// ACT
			verdict, err := binaryStep{}.Check(context.Background(), p)

			// ASSERT
			// Pending, not Blocked. This used to report "the service holds it
			// open" and demand --restart for ANY hash mismatch, without ever
			// asking the SCM — so the commonest case got a message that was
			// simply false, and clearing it bounced a service that was not up.
			require.NoError(t, err)
			assert.Equal(t, Pending, verdict.Condition)
			assert.NotContains(t, verdict.Detail, "--restart")
			assert.Empty(t, p.restartWanted, "nothing is running, so nothing needs restarting")

			// And it goes through: stopForReplace already tolerated both states.
			require.NoError(t, binaryStep{}.Apply(context.Background(), p))

			destSum, err := fileSum(p.BinPath)
			require.NoError(t, err)

			sourceSum, err := fileSum(opts.BinPath)
			require.NoError(t, err)
			assert.Equal(t, sourceSum, destSum)
		})
	}
}

func TestBinaryStep_ShouldReplaceTheBinaryWhenRestartWasAskedFor(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.Restart = true
	writeBinary(t, opts.Layout.BinDir, filepath.Base(opts.BinPath), "an older build")

	deps, m, _ := okDeps()
	m.Reported = runningStatus(opts)

	p := planFor(opts, deps)

	// ACT
	verdict, err := binaryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, binaryStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.Contains(t, m.Calls, "stop", "the binary was replaced without stopping the service first")

	destSum, err := fileSum(p.BinPath)
	require.NoError(t, err)

	sourceSum, err := fileSum(opts.BinPath)
	require.NoError(t, err)
	assert.Equal(t, sourceSum, destSum)

	// And the start step must know to bring it back up.
	assert.NotEmpty(t, p.restartWanted)
}

func TestBinaryStep_ShouldRegisterTheBinaryWhereItIsWhenCopyingIsRefused(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.NoCopy = true

	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := binaryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, binaryStep{}.Apply(context.Background(), p))

	// ASSERT
	// The gate uses this, and so does anyone installing from a location they
	// have already vetted. install still checks that directory's grants, so
	// the flag skips the copy and not the safety.
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.Equal(t, opts.BinPath, p.BinPath)
	assert.NoDirExists(t, opts.Layout.BinDir)
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// installPlan returns a plan whose config and binary steps have already run.
func installPlan(t *testing.T, opts Options, deps Deps) *Plan {
	t.Helper()

	p := planFor(opts, deps)

	_, err := configStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, configStep{}.Apply(context.Background(), p))

	_, err = binaryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, binaryStep{}.Apply(context.Background(), p))

	return p
}

func TestInstallStep_ShouldRegisterAnAbsentService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, installStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	require.Len(t, m.Installed, 1)
	assert.Equal(t, p.BinPath, m.Installed[0].BinPath)
	assert.Equal(t, []string{configFlag, p.ConfigPath}, m.Installed[0].Args)
}

func TestInstallStep_ShouldLeaveAMatchingRegistrationAlone(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The fixture is built from what INSTALL ACTUALLY REGISTERED, not from the
	// plan's own fields. Built from p.BinPath and p.ConfigPath it mirrored the
	// assertion — Check compares against exactly those — so it passed whether
	// or not the registered command and the compared one agreed, and that is
	// how a relative --config stayed permanently Blocked with every test green.
	opts := runOptions(t)
	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	require.NoError(t, installStep{}.Apply(context.Background(), p))
	require.Len(t, m.Installed, 1)

	registered := m.Installed[0]
	m.Reported = winsvc.ServiceStatus{
		Name:      registered.Name,
		Installed: true,
		Command:   append([]string{registered.BinPath}, registered.Args...),
	}

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)

	// ASSERT
	// Compared against Command, never BinPath: BinPath is the ImagePath with
	// the SCM's escaping still on it, and comparing it to a Definition's
	// unescaped values would need a re-implementation of EscapeArg.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
}

func TestInstallStep_ShouldMatchARegistrationThatDiffersOnlyInCase(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	require.NoError(t, installStep{}.Apply(context.Background(), p))
	require.Len(t, m.Installed, 1)

	registered := m.Installed[0]
	m.Reported = winsvc.ServiceStatus{
		Name:      registered.Name,
		Installed: true,
		Command: []string{
			strings.ToUpper(registered.BinPath),
			configFlag,
			strings.ToUpper(registered.Args[1]),
		},
	}

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)

	// ASSERT
	// Windows paths are case-insensitive, so an operator who spelled
	// --bin-dir "C:\Program Files\…" once and "c:\program files\…" the next
	// time named the same directory. Answering Blocked would demand --reinstall
	// to fix a difference that is not one.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
}

func TestInstallStep_ShouldBlockARegistrationThatDiffers(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	m.Reported = winsvc.ServiceStatus{
		Name:      opts.Definition.Name,
		Installed: true,
		Command:   []string{`C:\somewhere\else\adapter.exe`, configFlag, `C:\elsewhere\config.toml`},
	}

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)

	// ASSERT
	// Manager.Install returns ErrAlreadyInstalled and never reconfigures, so
	// drift is not something a run can quietly correct — and correcting it
	// silently would adopt a registration somebody else made.
	require.NoError(t, err)
	assert.Equal(t, Blocked, verdict.Condition)
	assert.Contains(t, verdict.Detail, "--reinstall")
	assert.Empty(t, m.Installed)
}

func TestInstallStep_ShouldBlockARegistrationItCannotReadBack(t *testing.T) {
	t.Parallel()

	// ARRANGE — installed, but the command line did not decompose. This is
	// also every non-Windows host, where the stub leaves Command nil.
	opts := runOptions(t)
	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	m.Reported = winsvc.ServiceStatus{
		Name:      opts.Definition.Name,
		Installed: true,
		BinPath:   `"C:\mangled`,
	}

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)

	// ASSERT
	// Cannot-confirm is not confirmed. Reporting Satisfied here would claim a
	// match nobody checked, against a registration whose ImagePath is already
	// the shape an operator investigates when a service will not start.
	require.NoError(t, err)
	assert.Equal(t, Blocked, verdict.Condition)
	assert.Contains(t, verdict.Detail, "could not be read back")
}

func TestInstallStep_ShouldReplaceARegistrationWhenReinstallWasAskedFor(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.Reinstall = true

	deps, m, _ := okDeps()
	p := installPlan(t, opts, deps)

	m.Reported = winsvc.ServiceStatus{
		Name:      opts.Definition.Name,
		Installed: true,
		Command:   []string{`C:\somewhere\else\adapter.exe`},
	}

	// ACT
	verdict, err := installStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, installStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.Contains(t, m.Calls, "uninstall", "the old registration was left in place")
	require.Len(t, m.Installed, 1)
	assert.Equal(t, p.BinPath, m.Installed[0].BinPath)
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

func TestStartStep_ShouldStartAStoppedService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()

	m.Reported = winsvc.ServiceStatus{Name: opts.Definition.Name, Installed: true, State: winsvc.StateStopped}

	p := planFor(opts, deps)

	// ACT
	verdict, err := startStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, startStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.Contains(t, m.Calls, "start")
}

func TestStartStep_ShouldLeaveARunningServiceAlone(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()

	m.Reported = winsvc.ServiceStatus{Name: opts.Definition.Name, Installed: true, State: winsvc.StateRunning}

	// ACT
	verdict, err := startStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// Nothing upstream changed, so bouncing a service that is serving weave
	// would be a short outage nobody asked for.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.NotContains(t, m.Calls, "start")
}

func TestStartStep_ShouldBlockARunningServiceThatNeedsARestart(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, m, _ := okDeps()

	m.Reported = winsvc.ServiceStatus{Name: opts.Definition.Name, Installed: true, State: winsvc.StateRunning}

	p := planFor(opts, deps)
	p.wantsRestart("a token was minted")

	// ACT
	verdict, err := startStep{}.Check(context.Background(), p)

	// ASSERT
	// A restart is an outage. It happens because somebody asked, and the
	// verdict says why it is needed — a minted token does nothing until the
	// service re-reads the store at startup.
	require.NoError(t, err)
	assert.Equal(t, Blocked, verdict.Condition)
	assert.Contains(t, verdict.Detail, "--restart")
	assert.Contains(t, verdict.Detail, "a token was minted")
}

func TestStartStep_ShouldStopBeforeStartingWhenRestarting(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.Restart = true

	deps, m, _ := okDeps()
	m.Reported = winsvc.ServiceStatus{Name: opts.Definition.Name, Installed: true, State: winsvc.StateRunning}

	p := planFor(opts, deps)
	p.wantsRestart("a token was minted")

	// ACT
	verdict, err := startStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, startStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.Equal(t, []string{"status", "stop", "start"}, callsAfterCheck(m),
		"a restart that never stopped is a service still running the old state")
}

// callsAfterCheck returns the manager's calls, which begin with the Check's
// own status query.
func callsAfterCheck(m *winsvctest.Manager) []string { return m.Calls }
