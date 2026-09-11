/*
Testing: service.go

Pending:

Tested:

	runService -> - TestRunService_ShouldRequireACommand
	              - TestRunService_ShouldPrintUsageForHelp
	              - TestRunService_ShouldRejectAnUnknownCommand
	runServiceInstall -> - TestRunServiceInstall_ShouldRefuseWithoutConsent: and
	                       must not touch the SCM while refusing.
	                     - TestRunServiceInstall_ShouldRequireAConfigPath: the
	                       service cannot start without one, so refusing here is
	                       the difference between an error at install time and a
	                       service that never starts.
	                     - TestRunServiceInstall_ShouldRegisterAnAbsoluteUnquotedDefinition
	                     - TestRunServiceInstall_ShouldPassTheDrainBudgetThrough
	                     - TestRunServiceInstall_ShouldReportAnAlreadyInstalledService
	runServiceLifecycle -> - TestRunServiceUninstall_ShouldRefuseWithoutYes
	                       - TestRunServiceLifecycle_ShouldCallTheMatchingOperation
	                       - TestRunServiceLifecycle_ShouldNameAnUninstalledService
	runServiceStatus -> - TestRunServiceStatus_ShouldReportAnAbsentService
	                    - TestRunServiceStatus_ShouldWarnWhenRecoveryIsInert
	                    - TestRunServiceStatus_ShouldReportAnInstalledService

Tested elsewhere:

	The SCM operations themselves, against a real Service Control Manager:
	task service-gate, and the M4a Phase -1 measurements that fixed their
	shape. internal/core/winsvc/service_windows.go has no unit coverage by
	design -- see its own doc block.

	That the third arm is reachable from the binary: main_test.go's dispatch
	tests.

Declined:

	Driving the real manager from here. NewManager needs Administrator and
	mutates the host; a unit test that did so would either fail on an
	unprivileged runner -- which is what ci:windows executes as -- or leave a
	service behind.

Additional Remarks:

	Every test here runs on every platform, which is the entire reason
	winsvc.Manager is an interface. The rules worth getting right in this file
	are refusals -- no consent, no config, no --yes -- and a refusal that only
	a Windows host could verify is a refusal nobody checks.

	fakeManager records calls rather than simulating the SCM. What these tests
	assert is what the command ASKED for; whether the SCM honours it is the
	gate's question and was measured before this code was written.
*/
package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// fakeManager records what the command asked the SCM to do.
type fakeManager struct {
	installed  []winsvc.Definition
	calls      []string
	status     winsvc.ServiceStatus
	installErr error
	opErr      error
	closed     bool
}

func (f *fakeManager) Install(d winsvc.Definition) error {
	f.calls = append(f.calls, "install")
	if f.installErr != nil {
		return f.installErr
	}

	f.installed = append(f.installed, d)

	return nil
}

func (f *fakeManager) Uninstall(string) error {
	f.calls = append(f.calls, "uninstall")

	return f.opErr
}

func (f *fakeManager) Start(string) error {
	f.calls = append(f.calls, "start")

	return f.opErr
}

func (f *fakeManager) Stop(string) error {
	f.calls = append(f.calls, "stop")

	return f.opErr
}

func (f *fakeManager) Status(string) (winsvc.ServiceStatus, error) {
	f.calls = append(f.calls, "status")

	return f.status, nil
}

func (f *fakeManager) Close() error {
	f.closed = true

	return nil
}

// factoryFor returns a managerFactory handing out m, and records whether it
// was ever called — a refusal that still opened an SCM connection has already
// done something it should not have.
func factoryFor(m *fakeManager, opened *bool) managerFactory {
	return func() (winsvc.Manager, error) {
		if opened != nil {
			*opened = true
		}

		return m, nil
	}
}

func TestRunService_ShouldRequireACommand(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	// ACT
	err := runService(nil, &out, factoryFor(&fakeManager{}, nil))

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, out.String(), "Usage:")
}

func TestRunService_ShouldPrintUsageForHelp(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"help", "-h", "--help"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var out bytes.Buffer

			// ACT
			err := runService([]string{verb}, &out, factoryFor(&fakeManager{}, nil))

			// ASSERT — asking for help is not a failure.
			require.NoError(t, err)
			assert.Contains(t, out.String(), "install")
		})
	}
}

func TestRunService_ShouldRejectAnUnknownCommand(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	// ACT
	err := runService([]string{"reinstall"}, &out, factoryFor(&fakeManager{}, nil))

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reinstall")
}

func TestRunServiceInstall_ShouldRefuseWithoutConsent(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var (
		out    bytes.Buffer
		opened bool
	)

	m := &fakeManager{}

	// ACT
	err := runService([]string{"install", "--config", "C:\\x\\config.toml"}, &out, factoryFor(m, &opened))

	// ASSERT
	// The refusal must come before the SCM connection: a command that opens a
	// privileged handle and then declines has already done the thing consent
	// was being asked about.
	require.Error(t, err)
	assert.False(t, opened, "the SCM was contacted despite the refusal")
	assert.Empty(t, m.installed)
	assert.Contains(t, out.String(), "LocalSystem")
	assert.Contains(t, out.String(), consentFlag)
}

func TestRunServiceInstall_ShouldRequireAConfigPath(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var (
		out    bytes.Buffer
		opened bool
	)

	// ACT
	err := runService([]string{"install", "--" + consentFlag}, &out, factoryFor(&fakeManager{}, &opened))

	// ASSERT
	// Without a config file the service has no way to receive
	// identity.namespaceKey, so it would fail on every single start. An error
	// here is the difference between the operator seeing it now and seeing it
	// in Event Viewer at 3am.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config is required")
	assert.Contains(t, err.Error(), "namespaceKey")
	assert.False(t, opened, "the SCM was contacted before the arguments were checked")
}

func TestRunServiceInstall_ShouldRegisterAnAbsoluteUnquotedDefinition(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}
	relative := filepath.Join("etc", "config.toml")

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", relative}, &out, factoryFor(m, nil))

	// ASSERT
	require.NoError(t, err)
	require.Len(t, m.installed, 1)

	def := m.installed[0]

	assert.True(t, filepath.IsAbs(def.BinPath), "the binary path must be absolute: under the SCM the "+
		"working directory is C:\\Windows\\System32")

	require.Len(t, def.Args, 2)
	assert.Equal(t, "--config", def.Args[0])
	assert.True(t, filepath.IsAbs(def.Args[1]), "the config path must be resolved before registration")

	// NOT quoted. The SCM registration escapes the path and every argument
	// itself; quoting here produces a doubly-escaped ImagePath and a service
	// that cannot start. Measured on Windows Server 2022: a spaced path
	// round-tripped correctly with no quoting on this side.
	assert.NotContains(t, def.BinPath, `"`)
	assert.NotContains(t, def.Args[1], `"`)
}

func TestRunServiceInstall_ShouldPassTheDrainBudgetThrough(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", "cfg.toml"}, &out, factoryFor(m, nil))

	// ASSERT
	// It becomes the service's PreshutdownTimeout. The binary owns one value
	// and hands it to both the server and the SCM; the two drifting apart is
	// what gets a legitimate drain killed.
	require.NoError(t, err)
	require.Len(t, m.installed, 1)
	assert.Equal(t, drainBudget, m.installed[0].DrainBudget)
	assert.Positive(t, m.installed[0].DrainBudget)
}

func TestRunServiceInstall_ShouldReportAnAlreadyInstalledService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{installErr: winsvc.ErrAlreadyInstalled}

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", "cfg.toml"}, &out, factoryFor(m, nil))

	// ASSERT
	require.ErrorIs(t, err, winsvc.ErrAlreadyInstalled)
	assert.True(t, m.closed, "the SCM connection was leaked")
}

func TestRunServiceUninstall_ShouldRefuseWithoutYes(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var (
		out    bytes.Buffer
		opened bool
	)

	m := &fakeManager{}

	// ACT
	err := runService([]string{"uninstall"}, &out, factoryFor(m, &opened))

	// ASSERT
	// The destructive half: it stops a running service, deletes the
	// registration and removes the Event Log source.
	require.Error(t, err)
	assert.False(t, opened)
	assert.Empty(t, m.calls)
	assert.Contains(t, out.String(), "--yes")
}

func TestRunServiceLifecycle_ShouldCallTheMatchingOperation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args []string
		want string
	}{
		"should start":     {args: []string{"start"}, want: "start"},
		"should stop":      {args: []string{"stop"}, want: "stop"},
		"should uninstall": {args: []string{"uninstall", "--yes"}, want: "uninstall"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var out bytes.Buffer

			m := &fakeManager{}

			// ACT
			err := runService(tc.args, &out, factoryFor(m, nil))

			// ASSERT
			require.NoError(t, err)
			assert.Equal(t, []string{tc.want}, m.calls)
			assert.True(t, m.closed, "the SCM connection was leaked")
			assert.Contains(t, out.String(), serviceName)
		})
	}
}

func TestRunServiceLifecycle_ShouldNameAnUninstalledService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{opErr: winsvc.ErrNotInstalled}

	// ACT
	err := runService([]string{"start"}, &out, factoryFor(m, nil))

	// ASSERT
	// The commonest mistake, and it reads as a typo in the service name if the
	// SCM's own error is passed through unchanged.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not installed")
	assert.Contains(t, err.Error(), serviceName)
}

func TestRunServiceStatus_ShouldReportAnAbsentService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{status: winsvc.ServiceStatus{Name: serviceName, Installed: false}}

	// ACT
	err := runService([]string{"status"}, &out, factoryFor(m, nil))

	// ASSERT — not installed is an answer, not an error.
	require.NoError(t, err)
	assert.Contains(t, out.String(), "not installed")
}

func TestRunServiceStatus_ShouldWarnWhenRecoveryIsInert(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{status: winsvc.ServiceStatus{
		Name:                serviceName,
		Installed:           true,
		State:               winsvc.StateRunning,
		StartType:           "automatic",
		RestartsOnCleanExit: false,
	}}

	// ACT
	err := runService([]string{"status"}, &out, factoryFor(m, nil))

	// ASSERT
	// The flag whose absence is otherwise invisible. Without it the SCM runs
	// recovery actions only when the process dies without reporting Stopped,
	// so the restart schedule shows up in services.msc and never fires.
	require.NoError(t, err)
	assert.Contains(t, out.String(), "WILL NOT restart")
	assert.Contains(t, out.String(), "inert")
}

func TestRunServiceStatus_ShouldReportAnInstalledService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{status: winsvc.ServiceStatus{
		Name:                serviceName,
		Installed:           true,
		State:               winsvc.StateRunning,
		StartType:           "automatic",
		BinPath:             `"C:\Program Files\wadapt\a.exe" --config "C:\x\c.toml"`,
		PreshutdownTimeout:  20 * time.Second,
		RestartsOnCleanExit: true,
	}}

	// ACT
	err := runService([]string{"status"}, &out, factoryFor(m, nil))

	// ASSERT
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "Running")
	assert.Contains(t, got, "automatic")
	// Shown verbatim, escaping included: a mangled ImagePath is exactly what
	// an operator is looking for when a service will not start.
	assert.Contains(t, got, `"C:\Program Files\wadapt\a.exe"`)
	assert.Contains(t, got, "20s")
	assert.NotContains(t, got, "WILL NOT restart")
}

func TestPreshutdownDescription_ShouldNameTheDefaultWhenUnset(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Contains(t, preshutdownDescription(0), "default")
	assert.Equal(t, "45s", preshutdownDescription(45*time.Second))
}
