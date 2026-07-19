/*
Testing: config.go

Pending:

Tested:

	Keys
	  - TestKeys_ShouldComposeWithTheCoreSpec: the adapter's keys and core's load
	    together — the first proof that key registration actually composes.
	  - TestKeys_ShouldLeaveTheIdentityKeysWithoutDefaults: neither identity input
	    may be defaulted, which is the whole mitigation for two fleet-wide re-key
	    risks.
	  - TestKeys_ShouldDeriveTheDocumentedEnvironmentNames: the dotted-key
	    convention the M3 plan writes down, pinned against the plan's own table.

	NewConfig
	  - TestNewConfig_ShouldApplyDefaults: the documented defaults land.
	  - TestNewConfig_ShouldCanonicalizeTheServerName: canonicalization happens once,
	    at load, not per derivation.
	  - TestNewConfig_ShouldReadEverySourceForTheIdentityKeys: both are settable from
	    the environment, which is the provisioning path for a backup-critical value.

	Validate
	  - TestValidate_ShouldRequireBothIdentityInputs: the adapter refuses to start
	    without either, and says why.
	  - TestValidate_ShouldRejectAProbeTimeoutThatIsNotShorter: the ordering rule
	    that keeps a slow backend from serializing health polls.
	  - TestValidate_ShouldRejectIncoherentPageSizes: a default above the maximum
	    would be clamped away on every request.
	  - TestValidate_ShouldReportEveryProblemAtOnce: joined, like the core's.

Tested elsewhere:

	The precedence machinery itself, and the loader's type coercion:
	  internal/core/config's tests. This file asserts only what the adapter adds.
	canonicalServerName's folding rules: identity_test.go.

Declined:

	A strength or entropy check on identity.namespaceKey: its job is
	  per-installation uniqueness rather than secrecy, so entropy is the wrong
	  measure. The length floor exists to catch a placeholder someone meant to
	  replace, and is documented as such rather than dressed up as security.

Additional Remarks:

	The identity keys carry most of this file's weight. Two of the plan's risk rows
	  — losing namespaceKey, and serverName drift — are mitigated entirely by these
	  keys having no defaults and no fallbacks, so a well-meaning future change
	  adding "a sensible default" is the failure this suite exists to block.
*/
package dhcpwindows

import (
	"testing"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadAdapter resolves the composed spec and builds the adapter config, which
// is exactly what the binary's wiring does.
func loadAdapter(t *testing.T, args []string) (Config, error) {
	t.Helper()

	values, err := config.Load(append(config.CoreKeys(), Keys()...), args)
	if err != nil {
		return Config{}, err
	}

	return NewConfig(values)
}

// loadAdapterEnv resolves with environment variables set for the test process.
// It cannot be parallel: it mutates the process environment.
func loadAdapterEnv(t *testing.T, args []string, env map[string]string) (Config, error) {
	t.Helper()

	for k, v := range env {
		t.Setenv(k, v)
	}

	values, err := config.Load(append(config.CoreKeys(), Keys()...), args)
	if err != nil {
		return Config{}, err
	}

	return NewConfig(values)
}

// identityArgs supplies the two required identity inputs as flags, so tests
// that are not about the environment can stay parallel.
func identityArgs(extra ...string) []string {
	return append([]string{
		"-identity-namespace-key", testNamespaceKey,
		"-identity-server-name", "dhcp01.example.test",
	}, extra...)
}

func TestKeys_ShouldComposeWithTheCoreSpec(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — core plus adapter, resolved in one pass. The loader
	// rejects a spec whose keys collide, so this also proves none of the eight
	// adapter keys collides with a core one.
	values, err := config.Load(append(config.CoreKeys(), Keys()...), identityArgs())
	require.NoError(t, err)

	core, err := config.Core(values)
	require.NoError(t, err)

	adapter, err := NewConfig(values)
	require.NoError(t, err)

	// ASSERT — one precedence pass serves both halves.
	assert.Equal(t, 8444, core.Port)
	assert.Equal(t, defaultPowerShellPath, adapter.PowerShellPath)
}

func TestKeys_ShouldLeaveTheIdentityKeysWithoutDefaults(t *testing.T) {
	t.Parallel()

	// ARRANGE
	required := map[string]bool{KeyNamespaceKey: true, KeyServerName: true}

	// ACT / ASSERT — this is the mitigation for two fleet-wide re-key risks, so
	// it is asserted on the registration rather than only on the behaviour. An
	// auto-generated namespaceKey that regenerates on reinstall *is* a
	// fleet-wide re-key; a serverName defaulting to os.Hostname() re-keys on a
	// host rename. Adding "a sensible default" here is the change this blocks.
	for _, k := range Keys() {
		if required[k.Name] {
			assert.Nil(t, k.Default, "%s must have no default", k.Name)

			continue
		}

		assert.NotNil(t, k.Default, "%s should carry a default", k.Name)
	}
}

func TestKeys_ShouldDeriveTheDocumentedEnvironmentNames(t *testing.T) {
	t.Parallel()

	// ARRANGE — the names as the M3 plan's config table documents them.
	want := map[string]string{
		KeyServer: "WEAVE_ADAPTER_DHCP_SERVER",
		// "powershellPath", not "powerShellPath": the segment is one word, so
		// it derives POWERSHELL_PATH rather than POWER_SHELL_PATH.
		KeyPowerShellPath:  "WEAVE_ADAPTER_DHCP_POWERSHELL_PATH",
		KeyCommandTimeout:  "WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT",
		KeyProbeTimeout:    "WEAVE_ADAPTER_DHCP_PROBE_TIMEOUT",
		KeyDefaultPageSize: "WEAVE_ADAPTER_SCOPES_DEFAULT_PAGE_SIZE",
		KeyMaxPageSize:     "WEAVE_ADAPTER_SCOPES_MAX_PAGE_SIZE",
		KeyNamespaceKey:    "WEAVE_ADAPTER_IDENTITY_NAMESPACE_KEY",
		KeyServerName:      "WEAVE_ADAPTER_IDENTITY_SERVER_NAME",
	}

	// ACT / ASSERT — these are operator-facing and documented, so they are
	// pinned rather than left to the derivation rule alone.
	for _, k := range Keys() {
		assert.Equal(t, want[k.Name], config.EnvName(k.Name), "key %s", k.Name)
	}
}

func TestNewConfig_ShouldApplyDefaults(t *testing.T) {
	t.Parallel()

	// ACT
	cfg, err := loadAdapter(t, identityArgs())

	// ASSERT — the documented defaults from the plan's config table.
	require.NoError(t, err)
	assert.Empty(t, cfg.Server, "empty means the local host")
	assert.Equal(t, "powershell.exe", cfg.PowerShellPath)
	assert.Equal(t, 10*time.Second, cfg.CommandTimeout)
	assert.Equal(t, 3*time.Second, cfg.ProbeTimeout)
	assert.Equal(t, 50, cfg.DefaultPageSize)
	assert.Equal(t, 500, cfg.MaxPageSize)
}

func TestNewConfig_ShouldCanonicalizeTheServerName(t *testing.T) {
	t.Parallel()

	// ACT — the forms an operator might plausibly write.
	cfg, err := loadAdapter(t, []string{
		"-identity-namespace-key", testNamespaceKey,
		"-identity-server-name", "  DHCP01.Example.TEST.  ",
	})

	// ASSERT — canonicalized once here, so that correcting the case of a config
	// value later does not re-derive every ID in the deployment.
	require.NoError(t, err)
	assert.Equal(t, "dhcp01.example.test", cfg.ServerName)
}

//nolint:paralleltest // t.Setenv mutates the process environment
func TestNewConfig_ShouldReadEverySourceForTheIdentityKeys(t *testing.T) {
	// ACT — the environment path, which is the standard answer for a
	// backup-critical value: config.toml enforces no file mode, and the derived
	// key convention gives an environment name for free.
	cfg, err := loadAdapterEnv(t, nil, map[string]string{
		"WEAVE_ADAPTER_IDENTITY_NAMESPACE_KEY": testNamespaceKey,
		"WEAVE_ADAPTER_IDENTITY_SERVER_NAME":   "DHCP02.example.test",
	})

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, testNamespaceKey, cfg.NamespaceKey)
	assert.Equal(t, "dhcp02.example.test", cfg.ServerName)
}

func TestValidate_ShouldRequireBothIdentityInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		wantMessage string
	}{
		{
			name:        "no namespace key",
			args:        []string{"-identity-server-name", "dhcp01.example.test"},
			wantMessage: "identity.namespaceKey is required",
		},
		{
			name:        "no server name",
			args:        []string{"-identity-namespace-key", testNamespaceKey},
			wantMessage: "identity.serverName is required",
		},
		{
			name: "a placeholder namespace key",
			args: []string{
				"-identity-namespace-key", "changeme",
				"-identity-server-name", "dhcp01.example.test",
			},
			wantMessage: "identity.namespaceKey must be at least",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := loadAdapter(t, tc.args)

			// ASSERT — the adapter refuses to start rather than inventing a
			// value, and the message says why it cannot be invented.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMessage)
		})
	}
}

func TestValidate_ShouldRejectAProbeTimeoutThatIsNotShorter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		probe string
	}{
		{name: "equal to the command timeout", probe: "10s"},
		{name: "longer than the command timeout", probe: "30s"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := loadAdapter(t, identityArgs("-dhcp-probe-timeout", tc.probe))

			// ASSERT — health.refresh holds its mutex across the probe, so the
			// probe's tighter bound is what stops aggressive polling
			// serializing behind one slow powershell.exe. A probe timeout at or
			// above the general one removes that protection while still looking
			// configured, which is why it is rejected rather than tolerated.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be shorter than")
		})
	}
}

func TestValidate_ShouldRejectIncoherentPageSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		wantMessage string
	}{
		{
			name:        "a default above the maximum",
			args:        []string{"-scopes-default-page-size", "600"},
			wantMessage: "must not exceed",
		},
		{
			name:        "a zero default",
			args:        []string{"-scopes-default-page-size", "0"},
			wantMessage: "scopes.defaultPageSize must be at least 1",
		},
		{
			name:        "a negative maximum",
			args:        []string{"-scopes-max-page-size", "-1"},
			wantMessage: "scopes.maxPageSize must be at least 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := loadAdapter(t, identityArgs(tc.args...))

			// ASSERT — a default above the maximum would be clamped away on
			// every request, so the configured value would silently never
			// apply.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMessage)
		})
	}
}

func TestValidate_ShouldReportEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE — several independent problems.
	cfg := Config{
		PowerShellPath:  "",
		CommandTimeout:  0,
		ProbeTimeout:    0,
		DefaultPageSize: 0,
		MaxPageSize:     0,
	}

	// ACT
	err := cfg.Validate()

	// ASSERT — joined, like the core's: an operator fixing configuration should
	// see all of it in one run rather than one problem per restart.
	require.Error(t, err)

	for _, want := range []string{
		"dhcp.powershellPath",
		"dhcp.commandTimeout",
		"dhcp.probeTimeout",
		"scopes.defaultPageSize",
		"scopes.maxPageSize",
		"identity.namespaceKey",
		"identity.serverName",
	} {
		assert.Contains(t, err.Error(), want)
	}
}
