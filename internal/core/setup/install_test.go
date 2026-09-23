/*
Testing: install.go

Pending:

Tested:
  Install
    - TestInstall_ShouldRegisterAnAbsoluteUnquotedDefinition: both paths resolved, neither quoted.
    - TestInstall_ShouldCarryTheDefinitionTemplateThrough: what the caller named is what gets registered.
    - TestInstall_ShouldRefuseAConfigurationThatWouldNotStart: service paths, core validation and the adapter's own, all reported at once.
    - TestInstall_ShouldCallTheSuppliedValidator: the adapter-bound seam is actually consulted.
    - TestInstall_ShouldRefuseAMissingConfigFile: a registration pointing at a file that is not there guarantees a failed start.
    - TestInstall_ShouldCheckTheDirectoryTheServiceWillRunFrom: the binary's directory, not the caller's.
    - TestInstall_ShouldRefuseAWritableBinaryDirectory: and register nothing.
    - TestInstall_ShouldSurfaceAnAlreadyInstalledService: and close the SCM connection anyway.
    - TestInstall_ShouldReportARegisteredServiceWhoseLockdownFailed: the error says the service exists.
    - TestInstall_ShouldReportEverySecuredTarget: results come back as data, not print.
  InstallOptions.check
    - TestInstall_ShouldRefuseIncompleteOptions: every missing field reported at once, before anything is touched.

Tested elsewhere:
  The wiring — that this adapter's real spec and real validator reach Install,
  and that the CLI renders what comes back:
  cmd/weave-adapter-dhcp-windows/service_test.go.

  The SCM operations themselves, against a live Service Control Manager: task
  service-gate.

Declined:
  Driving the real winsvc.Manager. NewManager needs Administrator and mutates
  the host; a unit test that did so would either fail on the unprivileged
  runner ci:windows executes as, or leave a service behind.

Additional Remarks:
  The spec these tests compose is SYNTHETIC — core's keys plus one invented
  "fake." key, with a validator that rejects it when unset. That is not
  convenience. Install exists in core, so it must be provable without an
  adapter in sight; a test that reached for dhcpwindows would compile only
  because of the import core is forbidden to have, and would stop proving the
  thing the package's shape is for.

  The path values are Windows-absolute on every host, because CheckServicePaths
  applies Windows rules everywhere — which is the point of it, and which is why
  t.TempDir() cannot be used for them even though it is used for the config
  file itself.

  The order Install works in is load and validate, then check the binary
  directory, then register, then secure. Each step's test asserts that nothing
  after it happened, because every one of them is a refusal whose value is
  entirely in coming BEFORE the registration.
*/

package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc/winsvctest"
)

// A stand-in adapter key, so these tests exercise the Validate seam without
// core importing an adapter. See the doc block.
const testAdapterKey = "fake.required"

// testRoot is a Windows-absolute directory that exists on no host.
//
// Windows-absolute because the service-path rule applies Windows rules
// everywhere, and UNIQUE because a fixed one is not a fixture on the machine
// this software is built for. The first version used
// C:\ProgramData\weave-adapters, which is not a made-up path on WS2022 — it is
// the live deployment. The token step's Check reads authTokensFile, so on the
// real host it loaded the REAL token store, found a usable token, and reported
// the mint already satisfied. Green on a developer machine, wrong on the only
// host that matters.
//
// The pid also keeps two test binaries running at once from sharing a fixture.
var testRoot = fmt.Sprintf(`C:\weave-adapters-test-%d`, os.Getpid())

// Paths for the config bodies below. Nothing creates or opens them: Install
// validates a configuration, it does not open the token store or the log.
var (
	testTokenStore = testRoot + `\tokens.toml`
	testLogFile    = testRoot + `\adapter.log`
)

// testSpec is core's keys plus one invented adapter key.
func testSpec() config.Spec {
	return append(config.CoreKeys(), config.Key{
		Name:  testAdapterKey,
		Type:  config.TypeString,
		Usage: "a stand-in adapter key, so Validate has something to reject",
	})
}

// validateTestAdapter is the shape InstallOptions.Validate takes: the check
// that only the adapter could perform.
func validateTestAdapter(v *config.Values) error {
	if v.String(testAdapterKey) == "" {
		return fmt.Errorf("%s is required and has no default", testAdapterKey)
	}

	return nil
}

// writeConfig writes a config file Install will accept, plus any extra body,
// and returns its path.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")

	body := "" +
		"logFile = '" + testLogFile + "'\n" +
		"authTokensFile = '" + testTokenStore + "'\n" +
		"[fake]\n" +
		"required = 'set'\n" +
		extra

	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// testDefinition is the registration template a caller supplies.
func testDefinition() winsvc.Definition {
	return winsvc.Definition{
		Name:        "wadapt-test",
		DisplayName: "weave test adapter",
		Description: "a service that exists only in this test",
		DrainBudget: 20 * time.Second,
	}
}

// testOptions is a complete, valid set of options over the given config.
func testOptions(t *testing.T, configPath string) InstallOptions {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "adapter.exe")
	require.NoError(t, os.WriteFile(binPath, []byte("not really a binary"), 0o600))

	return InstallOptions{
		Definition: testDefinition(),
		BinPath:    binPath,
		ConfigPath: configPath,
		Spec:       testSpec(),
		Validate:   validateTestAdapter,
	}
}

// runOptions is a complete, valid set of Options for a provisioning run.
//
// The layout lives in a real temp directory while the two path VALUES are
// Windows-shaped, which is not an inconsistency: CheckServicePaths applies
// Windows rules on every host — that is the point of it — so a config carrying
// a /tmp path would be refused, while the directory this process actually
// creates has to be one this host can create.
func runOptions(t *testing.T) Options {
	t.Helper()

	dir := t.TempDir()

	binPath := filepath.Join(t.TempDir(), "adapter.exe")
	require.NoError(t, os.WriteFile(binPath, []byte("not really a binary"), 0o600))

	return Options{
		Spec:            testSpec(),
		Definition:      testDefinition(),
		Validate:        validateTestAdapter,
		HealthComponent: testComponent,
		ProtectedPath:   "/api/v1/things",
		BinPath:         binPath,
		Layout: Layout{
			Dir:            dir,
			ConfigPath:     filepath.Join(dir, "config.toml"),
			TokenStorePath: testTokenStore,
			LogPath:        testLogFile,
			BinDir:         filepath.Join(dir, "bin"),
		},
		Provisioned: []config.Provisioned{
			{Key: config.KeyAuthTokensFile, Value: testTokenStore},
			{Key: config.KeyLogFile, Value: testLogFile},
			{Key: testAdapterKey, Value: "set"},
		},
		TokenLabel: "weave-prod",
		Now:        func() time.Time { return time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC) },
	}
}

// okDeps returns dependencies that accept everything, and the doubles to
// assert against.
//
// Get answers a healthy body for the component the run options name, so a test
// that is not about verification does not have to arrange one.
func okDeps() (Deps, *winsvctest.Manager, *winsvctest.Securer) {
	m := &winsvctest.Manager{}
	sec := &winsvctest.Securer{}

	return Deps{
		NewManager: func() (winsvc.Manager, error) { return m, nil },
		Secure:     sec.Secure,
		CheckDir:   func(string, winsvc.Policy) error { return nil },
		Get:        healthyGet(testComponent),
	}, m, sec
}

// testComponent is the health component the run options below require.
const testComponent = "backend"

// healthyGet answers a health body reporting the named component healthy, and
// 200 on anything else.
func healthyGet(component string) GetFunc {
	return componentGet(component, health.StatusHealthy)
}

// componentGet answers a health body reporting the named component with the
// given status.
func componentGet(component string, status health.Status) GetFunc {
	body, _ := json.Marshal(health.Response{
		Status:     status,
		Components: []health.Component{{Name: component, Status: status, Detail: "from the double"}},
	})

	return func(_ context.Context, url, _ string) (Response, error) {
		if strings.Contains(url, httpserver.HealthPath) {
			return Response{Status: http.StatusOK, Body: body}, nil
		}

		// A protected route on a host with no backend: not 401, which is the
		// only answer that would disprove what the check is establishing.
		return Response{Status: http.StatusBadGateway}, nil
	}
}

func TestInstall_ShouldRegisterAnAbsoluteUnquotedDefinition(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, _ := okDeps()
	opts := testOptions(t, writeConfig(t, ""))

	// ACT
	result, err := Install(opts, deps)

	// ASSERT
	require.NoError(t, err)
	require.Len(t, m.Installed, 1)

	def := m.Installed[0]

	assert.True(t, filepath.IsAbs(def.BinPath),
		"the binary path must be absolute: under the SCM the working directory is "+config.ServiceWorkingDirectory)

	require.Len(t, def.Args, 2)
	assert.Equal(t, "--config", def.Args[0])
	assert.True(t, filepath.IsAbs(def.Args[1]), "the config path must be resolved before registration")

	// NOT quoted. The SCM registration escapes the path and every argument
	// itself; quoting here produces a doubly-escaped ImagePath and a service
	// that cannot start. Measured on Windows Server 2022: a spaced path
	// round-tripped correctly with no quoting on this side.
	assert.NotContains(t, def.BinPath, `"`)
	assert.NotContains(t, def.Args[1], `"`)

	// The same values come back, so a caller can render what was registered
	// without re-deriving it and drifting.
	assert.Equal(t, def.BinPath, result.BinPath)
	assert.Equal(t, def.Args[1], result.ConfigPath)
	assert.Equal(t, def, result.Definition)
}

func TestInstall_ShouldCarryTheDefinitionTemplateThrough(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, _ := okDeps()
	opts := testOptions(t, writeConfig(t, ""))

	// ACT
	_, err := Install(opts, deps)

	// ASSERT
	// Core supplies none of this. The service name, what services.msc shows
	// and the drain budget come from the binary, which is the one place that
	// knows which adapter it is — and the budget in particular becomes the
	// PreshutdownTimeout, where drifting from the server's own drain is what
	// gets a legitimate shutdown killed.
	require.NoError(t, err)
	require.Len(t, m.Installed, 1)

	def := m.Installed[0]
	assert.Equal(t, "wadapt-test", def.Name)
	assert.Equal(t, "weave test adapter", def.DisplayName)
	assert.Equal(t, "a service that exists only in this test", def.Description)
	assert.Equal(t, 20*time.Second, def.DrainBudget)
}

func TestInstall_ShouldRefuseAConfigurationThatWouldNotStart(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body    string
		wantErr string
	}{
		"should refuse a relative token store": {
			// The likeliest one: it is the shipped default, so an operator who
			// never set the key gets it.
			body:    "authTokensFile = 'tokens.toml'\nlogFile = '" + testLogFile + "'\n[fake]\nrequired = 'set'\n",
			wantErr: "authTokensFile",
		},
		"should refuse a relative log file": {
			body: "authTokensFile = '" + testTokenStore + "'\nlogFile = 'logs\\\\adapter.log'\n" +
				"[fake]\nrequired = 'set'\n",
			wantErr: "logFile",
		},
		"should refuse a port the server would reject": {
			// Not a path rule, but the same principle: core's own validation
			// runs here, so a configuration the server would refuse is not
			// registered as a service that retries it three times first.
			body: "port = 0\nauthTokensFile = '" + testTokenStore + "'\nlogFile = '" + testLogFile + "'\n" +
				"[fake]\nrequired = 'set'\n",
			wantErr: "port",
		},
		"should refuse a configuration the adapter rejects": {
			body:    "authTokensFile = '" + testTokenStore + "'\nlogFile = '" + testLogFile + "'\n",
			wantErr: testAdapterKey,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))

			deps, m, sec := okDeps()

			// ACT
			_, err := Install(testOptions(t, path), deps)

			// ASSERT
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, m.Installed, "a service was registered for a configuration that cannot start")
			assert.Empty(t, sec.Targets, "files were secured for a service that was never registered")
		})
	}
}

func TestInstall_ShouldCallTheSuppliedValidator(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var seen *config.Values

	deps, _, _ := okDeps()

	opts := testOptions(t, writeConfig(t, ""))
	opts.Validate = func(v *config.Values) error {
		seen = v

		return nil
	}

	// ACT
	_, err := Install(opts, deps)

	// ASSERT
	// The seam exists because core must never import internal/adapters, so
	// this is the only way the adapter's own check reaches the install path.
	// A Validate that is accepted and never called would be worse than none:
	// it would read as covered.
	require.NoError(t, err)
	require.NotNil(t, seen, "the validator was never called")

	// Handed the FULLY RESOLVED values, not the arguments — which is what
	// catches a bad value set inside the TOML rather than on the command line.
	assert.Equal(t, "set", seen.String(testAdapterKey))
}

func TestInstall_ShouldRefuseAMissingConfigFile(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, _ := okDeps()
	absent := filepath.Join(t.TempDir(), "not-there.toml")

	// ACT
	_, err := Install(testOptions(t, absent), deps)

	// ASSERT
	// Registering a service that points at a file which is not there
	// guarantees a failed start, three SCM retries, and an operator reading
	// Event Viewer for something the installer could see.
	require.Error(t, err)
	assert.Empty(t, m.Installed)
}

func TestInstall_ShouldCheckTheDirectoryTheServiceWillRunFrom(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var (
		checked       string
		checkedPolicy winsvc.Policy
	)

	deps, _, _ := okDeps()
	opts := testOptions(t, writeConfig(t, ""))

	deps.CheckDir = func(dir string, policy winsvc.Policy) error {
		checked = dir
		checkedPolicy = policy

		return nil
	}

	// ACT
	_, err := Install(opts, deps)

	// ASSERT
	// The directory holding the executable that will be REGISTERED — not the
	// working directory and not the config's. What matters is where the SCM
	// will launch the binary from.
	require.NoError(t, err)
	assert.Equal(t, filepath.Dir(opts.BinPath), checked)

	// And at the weaker policy, deliberately. This directory is checked and
	// never repaired: %ProgramFiles% is owned by TrustedInstaller and a
	// subdirectory of it by whichever elevated operator created it, so
	// demanding SYSTEM or Administrators as the owner would refuse every
	// correct install. Foreign write is the escalation, and it is what is
	// refused.
	assert.Equal(t, winsvc.PolicyNoForeignWrite, checkedPolicy)
}

func TestInstall_ShouldRefuseAWritableBinaryDirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, sec := okDeps()
	deps.CheckDir = func(string, winsvc.Policy) error {
		return fmt.Errorf("%w: C:\\gate\\bin grants write to [S-1-5-32-545]", winsvc.ErrNotSecured)
	}

	// ACT
	_, err := Install(testOptions(t, writeConfig(t, "")), deps)

	// ASSERT
	// A LocalSystem service launched from a folder a non-admin can write is a
	// full escalation: replace the exe and Windows runs it as SYSTEM at the
	// next start. Refused before anything is registered.
	require.ErrorIs(t, err, winsvc.ErrNotSecured)
	assert.Contains(t, err.Error(), "LocalSystem")
	assert.Empty(t, m.Installed, "a service was registered despite a writable binary directory")
	assert.Empty(t, sec.Targets, "nothing should be secured once the install is refused")
}

func TestInstall_ShouldSurfaceAnAlreadyInstalledService(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, _ := okDeps()
	m.InstallErr = winsvc.ErrAlreadyInstalled

	// ACT
	_, err := Install(testOptions(t, writeConfig(t, "")), deps)

	// ASSERT
	// Wrapped, never flattened: "already installed" is the one an operator
	// most often hits, and a caller that cannot tell it apart cannot say so.
	require.ErrorIs(t, err, winsvc.ErrAlreadyInstalled)
	assert.True(t, m.Closed, "the SCM connection was leaked")
}

func TestInstall_ShouldReportARegisteredServiceWhoseLockdownFailed(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, sec := okDeps()
	sec.Err = winsvc.ErrNotSecured

	// ACT
	_, err := Install(testOptions(t, writeConfig(t, "")), deps)

	// ASSERT
	// The service exists by this point, and the error has to say so: an
	// operator told only "install failed" would reinstall, hit
	// ErrAlreadyInstalled, and leave the files wide open.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wadapt-test", "the error must name the service that now exists")
	assert.Contains(t, err.Error(), "is registered")
	assert.Contains(t, err.Error(), "securing its files failed")
	assert.Len(t, m.Installed, 1, "the registration did happen, whatever the error says")
}

func TestInstall_ShouldReportEverySecuredTarget(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, _ := okDeps()
	cfg := writeConfig(t, "")

	// ACT
	result, err := Install(testOptions(t, cfg), deps)

	// ASSERT
	// Returned rather than printed, because whether a skipped target deserves
	// a warning and in what words belongs to whichever command is speaking —
	// and one of these callers is an MSI custom action that reads an exit code.
	require.NoError(t, err)
	require.Len(t, result.Secured, 3)

	paths := make([]string, 0, len(result.Secured))
	for _, r := range result.Secured {
		paths = append(paths, r.Target.Path)
	}

	assert.Contains(t, paths, result.ConfigPath, "the config file carries the identity secret")
	assert.Contains(t, paths, testTokenStore)
	// The log DIRECTORY, not the log: the adapter creates the file at runtime
	// and it inherits the directory's entries.
	assert.Contains(t, paths, testRoot)
	assert.NotContains(t, paths, testLogFile)
}

func TestInstall_ShouldRefuseIncompleteOptions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate  func(*InstallOptions)
		wantErr string
	}{
		"should refuse a missing config path": {
			mutate:  func(o *InstallOptions) { o.ConfigPath = "" },
			wantErr: "ConfigPath is required",
		},
		"should refuse a missing binary path": {
			mutate:  func(o *InstallOptions) { o.BinPath = "" },
			wantErr: "BinPath is required",
		},
		"should refuse a missing validator": {
			mutate:  func(o *InstallOptions) { o.Validate = nil },
			wantErr: "Validate is required",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			deps, m, _ := okDeps()
			opts := testOptions(t, writeConfig(t, ""))
			tc.mutate(&opts)

			// ACT
			_, err := Install(opts, deps)

			// ASSERT
			// A nil Validate is the one that matters: it would not fail, it
			// would silently skip the adapter's check and register a service
			// that cannot start. Refused before anything is touched.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, m.Calls, "the SCM was contacted for an incomplete request")
		})
	}
}

func TestInstall_ShouldReportEveryMissingOptionAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE — nothing supplied at all.
	deps, _, _ := okDeps()

	// ACT
	_, err := Install(InstallOptions{}, deps)

	// ASSERT
	// Joined rather than first-wins, for the same reason the configuration
	// errors are: fixing one field only to be told about the next is the
	// version of this that wastes an afternoon.
	require.Error(t, err)

	for _, want := range []string{"ConfigPath is required", "BinPath is required", "Validate is required"} {
		assert.Contains(t, err.Error(), want)
	}
}
