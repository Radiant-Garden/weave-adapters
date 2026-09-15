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
	"fmt"
	"os"
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

// fakeSecurer records what the lockdown was asked to cover.
type fakeSecurer struct {
	targets []winsvc.Securable
	err     error
	skip    bool
}

func (f *fakeSecurer) secure(targets []winsvc.Securable) ([]winsvc.SecureResult, error) {
	f.targets = append(f.targets, targets...)

	if f.err != nil {
		return nil, f.err
	}

	results := make([]winsvc.SecureResult, 0, len(targets))
	for _, t := range targets {
		// Applied unless the fixture says otherwise, so a test asserting the
		// skipped-target message has to ask for it.
		results = append(results, winsvc.SecureResult{Target: t, Applied: !f.skip})
	}

	return results, nil
}

// depsFor returns serviceDeps handing out m, and records whether the SCM was
// ever contacted — a refusal that still opened a privileged handle has
// already done the thing it was refusing.
func depsFor(m *fakeManager, opened *bool) serviceDeps {
	return depsWith(m, opened, &fakeSecurer{})
}

// depsWith is depsFor with a caller-supplied securer, for the tests that
// assert on what was locked down.
func depsWith(m *fakeManager, opened *bool, sec *fakeSecurer) serviceDeps {
	return depsChecking(m, opened, sec, func(string) error { return nil })
}

// depsChecking is depsWith with a caller-supplied binary-directory check, for
// the tests that assert install refuses a writable one.
func depsChecking(m *fakeManager, opened *bool, sec *fakeSecurer, check checkDirFunc) serviceDeps {
	return serviceDeps{
		newManager: func() (winsvc.Manager, error) {
			if opened != nil {
				*opened = true
			}

			return m, nil
		},
		secure:   sec.secure,
		checkDir: check,
	}
}

// Windows-shaped paths for the config bodies below.
//
// They do not need to exist: install validates the configuration, it does not
// open the token store or the log. They DO need to be Windows-absolute,
// because the service-path rule applies Windows rules on every host -- which
// is the point, and which is why t.TempDir() cannot be used for these values
// even though it is used for the config file itself.
const (
	//nolint:gosec // G101: a path to the store, not a credential — the same exemption config.KeyAuthTokensFile carries.
	winTokenStore = `C:\ProgramData\weave-adapters\tokens.toml`
	winLogFile    = `C:\ProgramData\weave-adapters\adapter.log`
)

// writeServiceConfig writes a config file install will accept, and returns its
// path.
func writeServiceConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")

	body := "" +
		"logFile = '" + winLogFile + "'\n" +
		"authTokensFile = '" + winTokenStore + "'\n" +
		"[identity]\n" +
		"namespaceKey = 'install-namespace-key-0123456789'\n" +
		"serverName = 'dhcp01.install.test'\n" +
		extra

	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestRunService_ShouldRequireACommand(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	// ACT
	err := runService(nil, &out, depsFor(&fakeManager{}, nil))

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
			err := runService([]string{verb}, &out, depsFor(&fakeManager{}, nil))

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
	err := runService([]string{"reinstall"}, &out, depsFor(&fakeManager{}, nil))

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
	err := runService([]string{"install", "--config", "C:\\x\\config.toml"}, &out, depsFor(m, &opened))

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
	err := runService([]string{"install", "--" + consentFlag}, &out, depsFor(&fakeManager{}, &opened))

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

	// A relative --config, to prove the registration resolves it: the path the
	// SCM records must not depend on where the installer happened to be run.
	cfg := writeServiceConfig(t, "")
	rel, err := filepath.Rel(mustGetwd(t), cfg)
	require.NoError(t, err)

	// ACT
	err = runService([]string{"install", "--" + consentFlag, "--config", rel}, &out, depsFor(m, nil))

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
	err := runService([]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")},
		&out, depsFor(m, nil))

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
	err := runService([]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")},
		&out, depsFor(m, nil))

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
	err := runService([]string{"uninstall"}, &out, depsFor(m, &opened))

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
			err := runService(tc.args, &out, depsFor(m, nil))

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
	err := runService([]string{"start"}, &out, depsFor(m, nil))

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
	err := runService([]string{"status"}, &out, depsFor(m, nil))

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
	err := runService([]string{"status"}, &out, depsFor(m, nil))

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
	err := runService([]string{"status"}, &out, depsFor(m, nil))

	// ASSERT
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, "Running")
	assert.Contains(t, got, "automatic")
	// Shown verbatim, escaping included: a mangled ImagePath is exactly what
	// an operator is looking for when a service will not start.
	assert.Contains(t, got, `"C:\Program Files\wadapt\a.exe"`)
	assert.Contains(t, got, "20s")
	assert.Contains(t, got, "pre-shutdown", "the deadline is not the HTTP drain budget and must not read as it")
	assert.NotContains(t, got, "WILL NOT restart")
}

func TestPreshutdownDescription_ShouldNameTheDefaultWhenUnset(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Contains(t, preshutdownDescription(0), "default")
	assert.Equal(t, "45s", preshutdownDescription(45*time.Second))
}

// mustGetwd returns the working directory, failing the test rather than the
// caller having to handle an error that cannot happen in practice.
func mustGetwd(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	return wd
}

func TestRunServiceInstall_ShouldRefuseAConfigurationThatCannotStart(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body    string
		wantErr string
	}{
		"should refuse a relative token store": {
			body: "authTokensFile = 'tokens.toml'\n" +
				"[identity]\nnamespaceKey = 'install-namespace-key-0123456789'\nserverName = 'd.test'\n",
			// The trap the phase is named for, and the likeliest one: it is the
			// shipped default, so an operator who never set the key gets it.
			wantErr: "authTokensFile",
		},
		"should refuse a relative log file": {
			body: "logFile = 'logs\\\\adapter.log'\n" +
				"authTokensFile = '" + winTokenStore + "'\n" +
				"[identity]\nnamespaceKey = 'install-namespace-key-0123456789'\nserverName = 'd.test'\n",
			wantErr: "logFile",
		},
		"should refuse a relative powershell path": {
			body: "authTokensFile = '" + winTokenStore + "'\n" +
				"[dhcp]\npowershellPath = 'bin\\\\pwsh.exe'\n" +
				"[identity]\nnamespaceKey = 'install-namespace-key-0123456789'\nserverName = 'd.test'\n",
			wantErr: "dhcp.powershellPath",
		},
		"should refuse a missing identity key": {
			// Not a path rule, but the same principle: a configuration the
			// server would reject must not be registered as a service that
			// will retry it three times before anyone looks.
			body:    "authTokensFile = '" + winTokenStore + "'\n[identity]\nserverName = 'd.test'\n",
			wantErr: "identity.namespaceKey",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var out bytes.Buffer

			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))

			m := &fakeManager{}

			// ACT
			err := runService([]string{"install", "--" + consentFlag, "--config", path}, &out, depsFor(m, nil))

			// ASSERT
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, m.installed, "a service was registered for a configuration that cannot start")
		})
	}
}

func TestRunServiceInstall_ShouldAcceptABarePowerShellName(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}
	cfg := writeServiceConfig(t, "[dhcp]\npowershellPath = 'powershell.exe'\n")

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", cfg}, &out, depsFor(m, nil))

	// ASSERT
	// The shipped default. Go's LookPath does not search the working directory
	// on Windows, so a bare name cannot resolve against C:\Windows\System32 the
	// way a relative path does — and a rule that rejected it would be one
	// nobody could satisfy.
	require.NoError(t, err)
	assert.Len(t, m.installed, 1)
}

func TestRunServiceInstall_ShouldRefuseAMissingConfigFile(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}
	absent := filepath.Join(t.TempDir(), "not-there.toml")

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", absent}, &out, depsFor(m, nil))

	// ASSERT
	// Registering a service that points at a file which is not there
	// guarantees a failed start, three SCM retries, and an operator reading
	// Event Viewer for something the installer could see.
	require.Error(t, err)
	assert.Empty(t, m.installed)
}

func TestRunServiceInstall_ShouldSecureEverythingTheConfigurationNames(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	sec := &fakeSecurer{}
	cfg := writeServiceConfig(t, "")

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", cfg},
		&out, depsWith(&fakeManager{}, nil, sec))

	// ASSERT
	// All three, from the configuration the installer just resolved — which
	// is the argument for doing this in the binary rather than a script: a
	// script has to be told the paths and drifts from them silently.
	require.NoError(t, err)
	require.Len(t, sec.targets, 3)

	paths := make([]string, 0, len(sec.targets))
	for _, target := range sec.targets {
		paths = append(paths, target.Path)
	}

	assert.Contains(t, paths, cfg, "the config file carries identity.namespaceKey")
	assert.Contains(t, paths, winTokenStore)
	// The log DIRECTORY, not the log: the adapter creates the file at runtime
	// and it inherits the directory's entries.
	assert.Contains(t, paths, filepath.Dir(winLogFile))
	assert.NotContains(t, paths, winLogFile)
}

func TestRunServiceInstall_ShouldReportARegisteredServiceWhoseLockdownFailed(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	sec := &fakeSecurer{err: winsvc.ErrNotSecured}

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")},
		&out, depsWith(&fakeManager{}, nil, sec))

	// ASSERT
	// The service exists by this point, and the error has to say so: an
	// operator told only "install failed" would reinstall and hit
	// ErrAlreadyInstalled, with the files still wide open.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is registered")
	assert.Contains(t, err.Error(), "securing its files failed")
}

func TestRunServiceSecure_ShouldLockDownWithoutTouchingTheRegistration(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}
	sec := &fakeSecurer{}

	// ACT
	err := runService([]string{"secure", "--config", writeServiceConfig(t, "")},
		&out, depsWith(m, nil, sec))

	// ASSERT
	// The console deployment's one-command lockdown, and the repair path
	// after moving a store. It must not touch the SCM at all.
	require.NoError(t, err)
	assert.Len(t, sec.targets, 3)
	assert.Empty(t, m.calls, "secure must not touch the service registration")
}

func TestRunServiceSecure_ShouldRequireAConfigPath(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	sec := &fakeSecurer{}

	// ACT
	err := runService([]string{"secure"}, &out, depsWith(&fakeManager{}, nil, sec))

	// ASSERT
	// It is the config that names the other two paths, so there is nothing to
	// secure without it.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config is required")
	assert.Empty(t, sec.targets)
}

func TestRunServiceSecure_ShouldRefuseARelativeLogPath(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// A relative logFile makes the log directory ".", and securing that would
	// apply a protected, inheriting SYSTEM-and-Administrators list to
	// whatever directory the operator ran the command from.
	var out bytes.Buffer

	path := filepath.Join(t.TempDir(), "config.toml")
	body := "logFile = 'wadapt.log'\n" +
		"authTokensFile = '" + winTokenStore + "'\n" +
		"[identity]\nnamespaceKey = 'install-namespace-key-0123456789'\nserverName = 'd.test'\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	sec := &fakeSecurer{}

	// ACT
	err := runService([]string{"secure", "--config", path}, &out, depsWith(&fakeManager{}, nil, sec))

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logFile")
	assert.Empty(t, sec.targets, "nothing may be secured once a path is rejected")
}

func TestRunServiceInstall_ShouldSayWhenATargetWasSkipped(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	sec := &fakeSecurer{skip: true}

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")},
		&out, depsWith(&fakeManager{}, nil, sec))

	// ASSERT
	// Printing "secured" for a file that was never touched is worse than
	// printing nothing: it is what makes an operator believe the store is
	// locked after a later `token gen`.
	require.NoError(t, err)
	assert.Contains(t, out.String(), "NOT YET")
	assert.Contains(t, out.String(), "token gen")
}

func TestRunServiceInstall_ShouldRefuseAWritableBinaryDirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	m := &fakeManager{}
	sec := &fakeSecurer{}

	deps := depsChecking(m, nil, sec, func(string) error {
		return fmt.Errorf("%w: C:\\gate\\bin grants write to [S-1-5-32-545]", winsvc.ErrNotSecured)
	})

	// ACT
	err := runService([]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")},
		&out, deps)

	// ASSERT
	// A LocalSystem service launched from a folder a non-admin can write is a
	// full escalation: replace the exe and Windows runs it as SYSTEM at the
	// next start. Refused before anything is registered.
	require.ErrorIs(t, err, winsvc.ErrNotSecured)
	assert.Contains(t, err.Error(), "LocalSystem")
	assert.Empty(t, m.installed, "a service was registered despite a writable binary directory")
	assert.Empty(t, sec.targets, "nothing should be secured once the install is refused")
}

func TestRunServiceInstall_ShouldCheckTheDirectoryTheServiceWillRunFrom(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var (
		out     bytes.Buffer
		checked string
	)

	deps := depsChecking(&fakeManager{}, nil, &fakeSecurer{}, func(dir string) error {
		checked = dir

		return nil
	})

	// ACT
	require.NoError(t, runService(
		[]string{"install", "--" + consentFlag, "--config", writeServiceConfig(t, "")}, &out, deps))

	// ASSERT
	// The directory holding the executable, not the working directory and not
	// the config's: what matters is where the SCM will launch the binary from.
	self, err := os.Executable()
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(self), checked)
}
