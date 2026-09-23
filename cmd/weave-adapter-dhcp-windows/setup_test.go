/*
Testing: setup.go

Pending:

Tested:
  runSetup
    - TestRunSetup_ShouldTreatHelpAsSuccess
    - TestRunSetup_ShouldRefuseToMutateWithoutConsent: and not look at the host while refusing.
    - TestRunSetup_ShouldNotRequireConsentForADryRun: a gate on looking would become a reflex flag.
    - TestRunSetup_ShouldRequireATokenLabel
    - TestRunSetup_ShouldReportAFreshHostsPlanWithoutTouchingIt
  setupExitCode
    - TestSetupExitCode_ShouldGiveAnUnhealthyBackendItsOwnCode: the answer on every host without DHCP.
    - TestSetupExitCode_ShouldMapAnUnhealthyBackendToZeroWhenTolerated: the flag M4c's custom action needs.
  setupOptions
    - TestSetupOptions_ShouldDeriveEveryConfigFlagFromTheKeySet: no second vocabulary.
    - TestSetupOptions_ShouldNotOfferAFlagForTheNamespaceKey: NoFlag keeps it out of argv.
    - TestSetupOptions_ShouldDefaultTheLayoutPathsIntoTheProvisionedValues
    - TestSetupOptions_ShouldLetAnExplicitFlagWinOverTheLayoutDefault
  withNamespaceKey / guardRekey
    - TestWithNamespaceKey_ShouldRefuseBothChannelsAtOnce
    - TestWithNamespaceKey_ShouldGenerateOnATrulyFreshHost: and mark it so the fingerprint is shown.
    - TestGuardRekey_ShouldRefuseWhenTheDataDirectoryIsNotEmpty: a leftover store proves a key existed.
    - TestGuardRekey_ShouldRefuseWhenAServiceIsAlreadyRegistered
    - TestGuardRekey_ShouldRefuseAlongsideAnExistingConfig
    - TestGuardRekey_ShouldRefuseWhenTheServiceManagerCouldNotBeConsulted: "could not look" is not "nothing is there".
    - TestGuardRekey_ShouldNotTreatANonWindowsHostAsEvidence
  withLayoutPaths
    - TestWithLayoutPaths_ShouldLeaveAnOperatorsOwnConfigurationAlone: a default nobody typed would be refused as a disagreement.
    - TestWithLayoutPaths_ShouldProduceTheSameOrderEveryTime
  readNamespaceKey
    - TestReadNamespaceKey_ShouldTrimExactlyOneTrailingLineEnding: `openssl rand -hex 32 > key` leaves one.
    - TestReadNamespaceKey_ShouldDecodeTheEncodingsAShellProduces: PowerShell's > writes UTF-16LE with a BOM.
    - TestReadNamespaceKey_ShouldRefuseATruncatedUtf16File
    - TestReadNamespaceKey_ShouldRefuseAnEmptyFile
  equivalentAdapterValue
    - TestEquivalentAdapterValue_ShouldFoldTheServerNameButNothingElse: a canonicalized key folds; a path and a secret must not.

Tested elsewhere:
  Every provisioning decision: internal/core/setup. What is asserted here is
  this layer — the flags, the consent gate, the two secret channels, the
  re-key guard, and the exit-code table.

  The rendering of a plan and of a minted token: setupreport_test.go.

  That the arm is reachable from the binary at all: main_test.go's dispatch
  tests.

Declined:
  Driving a run to completion on this host. The install step needs a real SCM,
  and the fake manager these tests inject cannot make a service answer HTTP —
  the end-to-end provisioning of a live host is task service-gate's, which
  M4b Phase 2 extends to install through this command.

  Pinning that runSetup calls printHealth before its FAILING return, which is
  the wiring that makes the refused-token arm reachable. Reaching that arm
  needs a run that gets as far as verification and is then answered 401, and no
  such run is constructible off Windows: authTokensFile and logFile are
  FilePath keys, so CheckServicePaths demands Windows-absolute values on every
  host — and a run that supplied them would have tokenStep.Apply open
  `C:\…\tokens.toml`, creating a literal directory beside the package. Each
  half is covered instead: that a 401 fails the step, in
  internal/core/setup/verify_test.go, and that the arm renders what it should,
  in setupreport_test.go. The seam between them is the gate's.

Additional Remarks:
  The re-key guard is the one to keep honest, and its asymmetry is why it is
  tested from three directions. A wrong generate re-derives every wadaptID at
  once: weave sees every scope as gone, proposes to recreate each, and Windows
  refuses on the one-scope-per-subnet rule — fleet-wide sync paralysis that no
  rollback fixes, because the old key is gone. A false refusal is one message
  naming what it found. So the bar is deliberately high, and each of the three
  pieces of evidence it accepts gets its own test.

  The namespace key never appears in any assertion's expected value, and the
  tests that generate one assert on its FINGERPRINT or on its length. A test
  that hardcoded a key would be the one place in this repository where one is
  written down.
*/

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/setup"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc/winsvctest"
)

// setupDeps returns dependencies that accept everything, plus the doubles.
func setupDeps() (setup.Deps, *winsvctest.Manager, *winsvctest.Securer) {
	m := &winsvctest.Manager{}
	sec := &winsvctest.Securer{}

	return setup.Deps{
		NewManager: func() (winsvc.Manager, error) { return m, nil },
		Secure:     sec.Secure,
		CheckDir:   func(string, winsvc.Policy) error { return nil },
		Get: func(context.Context, string, string) (setup.Response, error) {
			return setup.Response{Status: 200, Body: []byte(`{"components":[]}`)}, nil
		},
	}, m, sec
}

// windowsLayoutArgs points a run at Windows-shaped directories that exist on
// no host.
//
// Windows-shaped because the service-path rule applies Windows rules
// everywhere, and unique because these tests run on the WS2022 host too — where
// a plausible-looking fixed path is a live deployment, not a fixture. The
// re-key guard reads the data directory, so a fixture that collided with a real
// one would exercise the opposite branch and pass while asserting the wrong
// thing.
func windowsLayoutArgs() []string {
	root := fmt.Sprintf(`C:\weave-adapters-test-%d`, os.Getpid())

	return []string{
		"--" + flagDataDir, root,
		"--" + flagBinDir, root + `\bin`,
	}
}

func TestRunSetup_ShouldTreatHelpAsSuccess(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"help", "-h", "--help"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var out bytes.Buffer

			deps, m, _ := setupDeps()

			// ACT
			result, err := runSetup(t.Context(), []string{verb}, &out, deps)

			// ASSERT — asking for help is not a failure.
			require.NoError(t, err)
			assert.Equal(t, exitOK, result.code)
			assert.Contains(t, out.String(), "Usage:")
			assert.Empty(t, m.Calls, "the host was inspected to print a usage message")
		})
	}
}

func TestRunSetup_ShouldRefuseToMutateWithoutConsent(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	deps, m, sec := setupDeps()

	// ACT
	result, err := runSetup(t.Context(),
		append([]string{"--" + flagTokenLabel, "weave-prod"}, windowsLayoutArgs()...), &out, deps)

	// ASSERT
	// The same gate `service install` applies, and for the same reason: a
	// privilege grant should not happen because somebody ran a command that
	// sounded routine.
	require.Error(t, err)
	assert.Equal(t, exitUsage, result.code)
	assert.Contains(t, out.String(), "LocalSystem")
	assert.Empty(t, m.Calls, "the SCM was contacted while refusing")
	assert.Empty(t, sec.Targets)
}

func TestRunSetup_ShouldNotRequireConsentForADryRun(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	deps, _, sec := setupDeps()

	args := append([]string{
		"--" + flagDryRun,
		"--" + flagTokenLabel, "weave-prod",
		"--" + config.FlagName(dhcpwindows.KeyServerName), "dhcp01.example.test",
		"--" + flagGenerateKey,
	}, windowsLayoutArgs()...)

	// ACT
	_, err := runSetup(t.Context(), args, &out, deps)

	// ASSERT
	// Consent gates MUTATION. Requiring an acknowledgement to look at a host
	// would make the flag a reflex rather than a decision — and --dry-run is
	// the command for "why will this not start", which an operator should
	// never have to consent to run.
	require.NoError(t, err)
	assert.NotContains(t, out.String(), "Re-run with --"+consentFlag)
	assert.Empty(t, sec.Targets, "a dry run locked something down")
}

func TestRunSetup_ShouldRequireATokenLabel(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	deps, _, _ := setupDeps()

	// ACT
	result, err := runSetup(t.Context(), append([]string{"--" + flagDryRun}, windowsLayoutArgs()...), &out, deps)

	// ASSERT
	// Store.Add refuses a duplicate label, so a re-run against a store whose
	// same-label token has expired fails at the mint. Making the operator name
	// the label is what makes that failure legible rather than mysterious.
	require.Error(t, err)
	assert.Equal(t, exitUsage, result.code)
	assert.Contains(t, err.Error(), flagTokenLabel)
}

func TestRunSetup_ShouldReportAFreshHostsPlanWithoutTouchingIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	deps, m, sec := setupDeps()

	args := append([]string{
		"--" + flagDryRun,
		"--" + flagTokenLabel, "weave-prod",
		"--" + config.FlagName(dhcpwindows.KeyServerName), "dhcp01.example.test",
		"--" + flagGenerateKey,
	}, windowsLayoutArgs()...)

	// ACT
	result, err := runSetup(t.Context(), args, &out, deps)

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, exitOK, result.code)

	// Every step named, so the output answers "what will this do" and, on a
	// broken host, "what is in the way".
	for _, step := range []string{"directory", "config", "token", "binary", "install", "start", "verify"} {
		assert.Contains(t, out.String(), step)
	}

	assert.Contains(t, out.String(), "Dry run")
	assert.Empty(t, m.Installed)
	assert.Empty(t, sec.Targets)
}

func TestSetupExitCode_ShouldGiveAnUnhealthyBackendItsOwnCode(t *testing.T) {
	t.Parallel()

	// ARRANGE — installed, running, and no DHCP backend behind it.
	result := setup.Result{Health: setup.HealthReport{Checked: true, Reachable: true}}

	// ACT / ASSERT
	// It is the outcome on every developer machine and every CI runner, and
	// automation has to be able to tell it from a step that failed. Collapsing
	// it into 1 would make a correct installation look broken; into 0 would
	// hide a backend that is genuinely down.
	assert.Equal(t, exitUnhealthy, setupExitCode(result, false))

	healthy := setup.Result{Health: setup.HealthReport{Checked: true, Reachable: true, ComponentHealthy: true}}
	assert.Equal(t, exitOK, setupExitCode(healthy, false))

	assert.Equal(t, exitOK, setupExitCode(setup.Result{DryRun: true}, false),
		"a dry run that found nothing blocked is a clean precheck")
}

func TestSetupExitCode_ShouldMapAnUnhealthyBackendToZeroWhenTolerated(t *testing.T) {
	t.Parallel()

	// ARRANGE
	result := setup.Result{Health: setup.HealthReport{Checked: true, Reachable: true}}

	// ACT / ASSERT
	// A WiX EXE custom action with Return="check" fails the whole install on
	// any non-zero code, so an MSI run on a host whose DHCP service is still
	// coming up would roll back. Return="ignore" was rejected because it also
	// discards 1 and 3, turning a genuinely failed install into a reported
	// success — this flag tolerates only the benign outcome.
	assert.Equal(t, exitOK, setupExitCode(result, true))
}

func TestSetupOptions_ShouldDeriveEveryConfigFlagFromTheKeySet(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	p := &printer{w: &out}
	deps, _, _ := setupDeps()

	args := append([]string{
		"--" + flagDryRun,
		"--" + flagTokenLabel, "weave-prod",
		// Derived names: these exist because the keys exist.
		"--" + config.FlagName(config.KeyPort), "9001",
		"--" + config.FlagName(dhcpwindows.KeyServerName), "dhcp01.example.test",
	}, windowsLayoutArgs()...)

	// ACT
	opts, _, _, err := setupOptions(args, p, deps)

	// ASSERT
	// No vocabulary of its own: --port exists because port is a key, and a
	// hand-written flag beside a key called identity.serverName would be a
	// second spelling nothing reconciles.
	require.NoError(t, err)
	assert.Contains(t, opts.Provisioned, config.Provisioned{Key: config.KeyPort, Value: 9001})
	assert.Contains(t, opts.Provisioned,
		config.Provisioned{Key: dhcpwindows.KeyServerName, Value: "dhcp01.example.test"})
}

func TestSetupOptions_ShouldNotOfferAFlagForTheNamespaceKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	p := &printer{w: &out}
	deps, _, _ := setupDeps()

	args := append([]string{
		"--" + flagDryRun,
		"--" + flagTokenLabel, "weave-prod",
		"--" + config.FlagName(dhcpwindows.KeyNamespaceKey), "a-key-on-the-command-line",
	}, windowsLayoutArgs()...)

	// ACT
	_, _, _, err := setupOptions(args, p, deps)

	// ASSERT
	// NoFlag is inherited from the key's registration rather than restated
	// here, which is the whole point of deriving the flag set: a flag value is
	// an argv entry any local user can read from a process listing, and this
	// key is backup-critical.
	require.Error(t, err)
	assert.Contains(t, out.String()+err.Error(), config.FlagName(dhcpwindows.KeyNamespaceKey))
}

func TestSetupOptions_ShouldDefaultTheLayoutPathsIntoTheProvisionedValues(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	p := &printer{w: &out}
	deps, _, _ := setupDeps()

	args := append([]string{"--" + flagDryRun, "--" + flagTokenLabel, "weave-prod"}, windowsLayoutArgs()...)

	// ACT
	opts, _, _, err := setupOptions(args, p, deps)

	// ASSERT
	// logFile above all. The SCM discards stdout, so a service without one
	// runs correctly and logs nowhere — which somebody discovers during an
	// incident.
	require.NoError(t, err)
	assert.Contains(t, opts.Provisioned,
		config.Provisioned{Key: config.KeyLogFile, Value: opts.Layout.LogPath})
	assert.Contains(t, opts.Provisioned,
		config.Provisioned{Key: config.KeyAuthTokensFile, Value: opts.Layout.TokenStorePath})
}

func TestSetupOptions_ShouldLetAnExplicitFlagWinOverTheLayoutDefault(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var out bytes.Buffer

	p := &printer{w: &out}
	deps, _, _ := setupDeps()

	elsewhere := `C:\logs\somewhere-else.log`

	args := append([]string{
		"--" + flagDryRun,
		"--" + flagTokenLabel, "weave-prod",
		"--" + config.FlagName(config.KeyLogFile), elsewhere,
	}, windowsLayoutArgs()...)

	// ACT
	opts, _, _, err := setupOptions(args, p, deps)

	// ASSERT
	require.NoError(t, err)
	assert.Contains(t, opts.Provisioned, config.Provisioned{Key: config.KeyLogFile, Value: elsewhere})

	for _, v := range opts.Provisioned {
		if v.Key == config.KeyLogFile {
			assert.Equal(t, elsewhere, v.Value, "the layout default overrode what the operator typed")
		}
	}
}

func TestWithNamespaceKey_ShouldRefuseBothChannelsAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, _ := setupDeps()
	layout := setup.Layout{Dir: t.TempDir()}

	// ACT
	_, err := withNamespaceKey(nil, "some-file", true, "", layout, deps)

	// ASSERT
	// Two channels exist because neither echoes; passing both says nothing
	// about which the operator meant, and guessing at a backup-critical secret
	// is not a guess worth making.
	require.Error(t, err)
	assert.Contains(t, err.Error(), flagNamespaceKeyFil)
	assert.Contains(t, err.Error(), flagGenerateKey)
}

func TestWithNamespaceKey_ShouldGenerateOnATrulyFreshHost(t *testing.T) {
	t.Parallel()

	// ARRANGE — an absent directory and no registration.
	deps, _, _ := setupDeps()
	layout := setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}

	// ACT
	provisioned, err := withNamespaceKey(nil, "", true, "", layout, deps)

	// ASSERT
	require.NoError(t, err)
	require.Len(t, provisioned, 1)

	value := provisioned[0]
	assert.Equal(t, dhcpwindows.KeyNamespaceKey, value.Key)

	// Both flags matter downstream: Secret keeps it out of every report, and
	// FreshlyGenerated is what the "back this file up" advice keys off.
	assert.True(t, value.Secret)
	assert.True(t, value.FreshlyGenerated)

	key, ok := value.Value.(string)
	require.True(t, ok)
	assert.Len(t, key, generatedKeyBytes*2, "hex of %d bytes", generatedKeyBytes)
}

func TestGuardRekey_ShouldRefuseWhenTheDataDirectoryIsNotEmpty(t *testing.T) {
	t.Parallel()

	// ARRANGE — a leftover token store, with no config anywhere.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokens.toml"), []byte(""), 0o600))

	deps, _, _ := setupDeps()

	// ACT
	err := guardRekey("", setup.Layout{Dir: dir}, deps)

	// ASSERT
	// A leftover store or log proves a key existed on this host even when the
	// config has been deleted — and generating a fresh one re-derives every ID
	// weave has seen, which no rollback undoes because the old key is gone.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokens.toml", "a refusal has to name what it found")
	assert.Contains(t, err.Error(), flagNamespaceKeyFil, "and the way forward")
}

func TestGuardRekey_ShouldRefuseWhenAServiceIsAlreadyRegistered(t *testing.T) {
	t.Parallel()

	// ARRANGE — an empty directory, and a service that is nonetheless there.
	deps, m, _ := setupDeps()
	m.Reported = winsvc.ServiceStatus{Name: serviceName, Installed: true}

	// ACT
	err := guardRekey("", setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}, deps)

	// ASSERT
	// A registration proves a key existed even when the whole directory has
	// been deleted, which is the case an "is the directory empty" check alone
	// would wave through.
	require.Error(t, err)
	assert.Contains(t, err.Error(), serviceName)
}

func TestGuardRekey_ShouldRefuseAlongsideAnExistingConfig(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, _ := setupDeps()

	// ACT
	err := guardRekey(`C:\theirs\config.toml`, setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}, deps)

	// ASSERT
	// An existing configuration already carries a key. Generating a second one
	// and writing it nowhere would be harmless; the danger is the operator who
	// believes the generated key is now this host's.
	require.Error(t, err)
	assert.Contains(t, err.Error(), flagGenerateKey)
}

func TestGuardRekey_ShouldAllowGeneratingOnATrulyFreshHost(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, _ := setupDeps()

	// ACT / ASSERT
	// Both the absent and the present-but-empty forms: an operator who created
	// the directory ahead of time has not thereby provisioned a key.
	require.NoError(t, guardRekey("", setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}, deps))
	require.NoError(t, guardRekey("", setup.Layout{Dir: t.TempDir()}, deps))
}

func TestGuardRekey_ShouldRefuseWhenTheServiceManagerCouldNotBeConsulted(t *testing.T) {
	t.Parallel()

	// ARRANGE — an SCM that refused, as it does for an unelevated process.
	deps, _, _ := setupDeps()
	deps.NewManager = func() (winsvc.Manager, error) {
		return nil, errors.New("access is denied")
	}

	// ACT
	err := guardRekey("", setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}, deps)

	// ASSERT
	// "Could not look" is not "nothing is there". An SCM that refused an
	// unelevated process says nothing about what is registered, and the
	// guard's asymmetry decides the rest: a wrong generate is fleet-wide sync
	// paralysis no rollback undoes.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be consulted")
}

func TestGuardRekey_ShouldNotTreatANonWindowsHostAsEvidence(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, _ := setupDeps()
	deps.NewManager = func() (winsvc.Manager, error) { return nil, winsvc.ErrUnsupported }

	// ACT / ASSERT
	// Categorically different from a refusal: there is no Windows service on a
	// host that has no Service Control Manager, so the directory check is the
	// whole of the evidence and it already passed.
	require.NoError(t, guardRekey("", setup.Layout{Dir: filepath.Join(t.TempDir(), "absent")}, deps))
}

func TestWithLayoutPaths_ShouldLeaveAnOperatorsOwnConfigurationAlone(t *testing.T) {
	t.Parallel()

	// ARRANGE
	//nolint:gosec // G101: paths to a store, not credentials.
	layout := setup.Layout{
		TokenStorePath: `C:\ProgramData\weave-adapters\tokens.toml`,
		LogPath:        `C:\ProgramData\weave-adapters\adapter.log`,
	}

	// ACT
	withConfig := withLayoutPaths(nil, layout, `C:	heirs\config.toml`)
	withoutConfig := withLayoutPaths(nil, layout, "")

	// ASSERT
	// An existing configuration is never rewritten and a provisioned value
	// that disagrees with it is refused — so injecting a default the operator
	// never typed would turn "their logFile is somewhere else" into a refusal
	// of a flag they did not pass.
	assert.Empty(t, withConfig)
	require.Len(t, withoutConfig, 2)

	// The VALUES, not only the keys. A fixture whose paths were mangled would
	// satisfy a key-only assertion while provisioning nonsense.
	assert.Contains(t, withoutConfig,
		config.Provisioned{Key: config.KeyLogFile, Value: layout.LogPath})
	assert.Contains(t, withoutConfig,
		config.Provisioned{Key: config.KeyAuthTokensFile, Value: layout.TokenStorePath})
	assert.True(t, config.IsAbsoluteServicePath(layout.LogPath), "the fixture is not a usable service path")
}

func TestWithLayoutPaths_ShouldProduceTheSameOrderEveryTime(t *testing.T) {
	t.Parallel()

	// ARRANGE
	//nolint:gosec // G101: paths to a store, not credentials.
	layout := setup.Layout{
		TokenStorePath: `C:\ProgramData\weave-adapters\tokens.toml`,
		LogPath:        `C:\ProgramData\weave-adapters\adapter.log`,
	}

	// ACT — repeated, because the failure it guards is a map's iteration order
	// and a single pass would pass by luck roughly half the time.
	first := keysOf(withLayoutPaths(nil, layout, ""))

	for range 20 {
		assert.Equal(t, first, keysOf(withLayoutPaths(nil, layout, "")))
	}

	// ASSERT
	// Render writes keys in the order it is handed them, so two runs given the
	// same values have to produce the same file — otherwise a diff between two
	// provisioned hosts shows a change nobody made.
	assert.Equal(t, []string{config.KeyAuthTokensFile, config.KeyLogFile}, first)
}

// keysOf names the provisioned keys in order.
func keysOf(provisioned []config.Provisioned) []string {
	out := make([]string, 0, len(provisioned))
	for _, v := range provisioned {
		out = append(out, v.Key)
	}

	return out
}

func TestReadNamespaceKey_ShouldTrimExactlyOneTrailingLineEnding(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		contents string
		want     string
	}{
		"should read a bare key":                  {contents: "a-provisioned-key", want: "a-provisioned-key"},
		"should trim a unix line ending":          {contents: "a-provisioned-key\n", want: "a-provisioned-key"},
		"should trim a windows line ending":       {contents: "a-provisioned-key\r\n", want: "a-provisioned-key"},
		"should keep a second trailing newline":   {contents: "a-provisioned-key\n\n", want: "a-provisioned-key\n"},
		"should keep whitespace inside the value": {contents: "a key with spaces\n", want: "a key with spaces"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			path := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(path, []byte(tc.contents), 0o600))

			// ACT
			got, err := readNamespaceKey(path)

			// ASSERT
			// `openssl rand -hex 32 > key` leaves exactly one newline, and an
			// untrimmed one becomes part of the key — so the fingerprint never
			// matches what the operator computes by hand and every derived ID
			// is silently wrong. Trimming MORE than one would be its own trap:
			// whitespace inside a deliberately chosen key is the operator's.
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReadNamespaceKey_ShouldDecodeTheEncodingsAShellProduces(t *testing.T) {
	t.Parallel()

	const key = "a-provisioned-namespace-key"

	// Encoded through utf16.Encode rather than by truncating runes, so the
	// fixture is what a real encoder emits rather than what ASCII happens to
	// make look the same.
	encode := func(s string, bom []byte, order binary.ByteOrder) []byte {
		out := append([]byte{}, bom...)

		var pair [2]byte

		for _, unit := range utf16.Encode([]rune(s)) {
			order.PutUint16(pair[:], unit)
			out = append(out, pair[:]...)
		}

		return out
	}

	utf16le := func(s string, bom []byte) []byte { return encode(s, bom, binary.LittleEndian) }
	utf16be := func(s string, bom []byte) []byte { return encode(s, bom, binary.BigEndian) }

	tests := map[string][]byte{
		"should read plain ascii":                        []byte(key),
		"should strip a utf-8 bom":                       append([]byte{0xEF, 0xBB, 0xBF}, key...),
		"should decode utf-16le with bom":                utf16le(key, []byte{0xFF, 0xFE}),
		"should decode utf-16be with bom":                utf16be(key, []byte{0xFE, 0xFF}),
		"should decode utf-16le with a trailing newline": utf16le(key+"\r\n", []byte{0xFF, 0xFE}),
	}

	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			path := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(path, contents, 0o600))

			// ACT
			got, err := readNamespaceKey(path)

			// ASSERT
			// UTF-16LE with a BOM is what PowerShell 5.1's `>` and Out-File
			// produce, which is the obvious way to write this file on the only
			// platform this adapter runs on. Read as bytes that is a NUL
			// between every character — the key would be silently wrong, every
			// derived ID would move, and the only symptom would be a
			// fingerprint mismatch with nothing naming the encoding.
			require.NoError(t, err)
			assert.Equal(t, key, got)
		})
	}
}

func TestReadNamespaceKey_ShouldRefuseATruncatedUtf16File(t *testing.T) {
	t.Parallel()

	// ARRANGE — a UTF-16 BOM and an odd number of bytes after it.
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte{0xFF, 0xFE, 'a', 0x00, 'b'}, 0o600))

	// ACT
	_, err := readNamespaceKey(path)

	// ASSERT
	// Refused rather than decoded to something. A truncated key file would
	// otherwise yield a key that is almost right, which re-derives every ID
	// exactly as thoroughly as one that is wholly wrong.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UTF-16")
}

func TestEquivalentAdapterValue_ShouldFoldTheServerNameButNothingElse(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		key                   string
		provisioned, resolved any
		want                  bool
	}{
		"should fold case in the server name": {
			key:         dhcpwindows.KeyServerName,
			provisioned: "win-01.radiantgarden.org", resolved: "WIN-01.radiantgarden.org", want: true,
		},
		"should fold a trailing dot in the server name": {
			key:         dhcpwindows.KeyServerName,
			provisioned: "WIN-01.radiantgarden.org.", resolved: "WIN-01.radiantgarden.org", want: true,
		},
		"should still see a genuinely different server name": {
			key:         dhcpwindows.KeyServerName,
			provisioned: "win-02.radiantgarden.org", resolved: "WIN-01.radiantgarden.org",
		},
		"should not fold case in a path": {
			key:         config.KeyLogFile,
			provisioned: `c:\logs\adapter.log`, resolved: `C:\logs\adapter.log`,
		},
		"should not fold case in the namespace key": {
			key:         dhcpwindows.KeyNamespaceKey,
			provisioned: "abcdef0123456789", resolved: "ABCDEF0123456789",
		},
		"should compare a non-string value directly": {
			key: config.KeyPort, provisioned: 8444, resolved: 8444, want: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			// Folding is the exception, not the rule. identity.serverName is
			// canonicalized before it is hashed, so two spellings are one
			// identity and refusing a re-run over the difference refuses a
			// change that is not a change. Folding a path or a key would do
			// the opposite: hide a real difference behind a false match.
			assert.Equal(t, tc.want, equivalentAdapterValue(tc.key, tc.provisioned, tc.resolved))
		})
	}
}

func TestReadNamespaceKey_ShouldRefuseAnEmptyFile(t *testing.T) {
	t.Parallel()

	// ARRANGE — what `touch key` leaves, and what a failed generate leaves.
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("\n"), 0o600))

	// ACT
	_, err := readNamespaceKey(path)

	// ASSERT
	// An empty key would be refused by the adapter's own validation later, but
	// by then a config file has been written around it.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// assertNoKeyInOutput is used by the tests above that print a report, and is
// the one assertion that must never be relaxed.
func assertNoKeyInOutput(t *testing.T, out, key string) {
	t.Helper()

	assert.NotContains(t, out, key,
		"identity.namespaceKey reached the output; PowerShell transcription and MSI logs both persist it")
}
