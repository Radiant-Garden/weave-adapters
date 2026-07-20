/*
Testing: config.go

Pending:

Tested:

	CoreKeys
	  - TestCoreKeys_ShouldPreserveTheEstablishedFlagAndEnvNames: the compatibility
	    constraint on the loader restructure — every core key still resolves to the
	    flag and environment variable it had before key registration existed.
	  - TestCoreKeys_ShouldDeclareEveryKeyItsStructReads: Core reads exactly the
	    keys CoreKeys registers, so neither half can drift without failing.

	Core
	  - TestCore_ShouldBuildFromResolvedValues: a resolved Values becomes a Config.
	  - TestCore_ShouldReturnTheValidationError: Core validates, so an invalid
	    combination never reaches a caller as a usable struct.

	Validate / fieldError
	  - TestValidate_ShouldReportProblems: valid and invalid configs, including all errors joined.
	  - TestValidate_ShouldRejectEmptyTokensFileWhenAuthEnabled: no silent empty allow-list.
	  - TestValidate_ShouldAllowEmptyTokensFileWhenAuthDisabled: the dev hatch needs no file.
	  - TestValidate_ShouldRejectAnUnboundedRequestBody: 0 and negative are startup
	    errors, not an "unlimited" setting.

Tested elsewhere:

	Precedence, type coercion and the environment/flag plumbing: loader_test.go.
	Name derivation and per-type parse errors: key_test.go.

Declined:

	Validate's non-ValidationErrors branch and fieldError's default case: defensive
	  and unreachable today (only *Config is validated, and every tagged field —
	  Port, LogSeverity, MaxRequestBodyBytes — has a bespoke message), so they are
	  documented in-code rather than tested.

Additional Remarks:

	TestCoreKeys_ShouldPreserveTheEstablishedFlagAndEnvNames is the regression the
	restructure most needed: flag and environment names are now *derived* from the
	key name rather than written out per key, so a change to the derivation rule
	would silently rename every operator-facing flag at once.
*/
package config

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoreKeys_ShouldPreserveTheEstablishedFlagAndEnvNames(t *testing.T) {
	t.Parallel()

	// ARRANGE — the names as they were hand-written before key registration.
	want := map[string]struct{ flag, env string }{
		KeyPort:           {"port", "WEAVE_ADAPTER_PORT"},
		KeyDisableHTTPS:   {"disable-https", "WEAVE_ADAPTER_DISABLE_HTTPS"},
		KeyLogSeverity:    {"log-severity", "WEAVE_ADAPTER_LOG_SEVERITY"},
		KeyAuthTokensFile: {"auth-tokens-file", "WEAVE_ADAPTER_AUTH_TOKENS_FILE"},
		KeyDisableAuth:    {"disable-auth", "WEAVE_ADAPTER_DISABLE_AUTH"},

		KeyMaxRequestBodyBytes: {"max-request-body-bytes", "WEAVE_ADAPTER_MAX_REQUEST_BODY_BYTES"},
	}

	// ACT / ASSERT
	keys := CoreKeys()
	require.Len(t, keys, len(want))

	for _, k := range keys {
		names, known := want[k.Name]
		require.True(t, known, "unexpected core key %q", k.Name)

		assert.Equal(t, names.flag, FlagName(k.Name))
		assert.Equal(t, names.env, EnvName(k.Name))
	}
}

func TestCoreKeys_ShouldDeclareEveryKeyItsStructReads(t *testing.T) {
	t.Parallel()

	// ARRANGE — Core panics on an unregistered key, so resolving the core spec
	// and building from it proves the two halves agree.
	values, err := load(CoreKeys(), nil, environ())
	require.NoError(t, err)

	// ACT / ASSERT
	cfg, err := Core(values)
	require.NoError(t, err)
	assert.NotNil(t, cfg)
}

func TestCore_ShouldBuildFromResolvedValues(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values, err := load(CoreKeys(), []string{"-port", "9100", "-log-severity", "warn"}, environ())
	require.NoError(t, err)

	// ACT
	cfg, err := Core(values)

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 9100, cfg.Port)
	assert.Equal(t, "warn", cfg.LogSeverity)
	assert.True(t, cfg.DisableHTTPS)
	assert.Equal(t, DefaultAuthTokensFile, cfg.AuthTokensFile)
	assert.False(t, cfg.DisableAuth)
}

func TestCore_ShouldReturnTheValidationError(t *testing.T) {
	t.Parallel()

	// ARRANGE — loads cleanly (it is a well-formed boolean) but does not validate.
	values, err := load(CoreKeys(), nil, environ("WEAVE_ADAPTER_DISABLE_HTTPS=false"))
	require.NoError(t, err)

	// ACT
	cfg, err := Core(values)

	// ASSERT
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "disableHttps must be true")
}

func TestValidate_ShouldReportProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr []string // substrings expected in the joined error; empty => no error
	}{
		{
			name: "valid config",
			cfg:  Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: 1, AuthTokensFile: "tokens.toml"},
		},
		{
			name:    "port too low",
			cfg:     Config{Port: 0, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: 1, AuthTokensFile: "tokens.toml"},
			wantErr: []string{"port must be between"},
		},
		{
			name:    "port too high",
			cfg:     Config{Port: 70000, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: 1},
			wantErr: []string{"port must be between"},
		},
		{
			name:    "unknown severity",
			cfg:     Config{Port: 8444, DisableHTTPS: true, LogSeverity: "trace", MaxRequestBodyBytes: 1},
			wantErr: []string{"logSeverity must be"},
		},
		{
			name:    "https enabled",
			cfg:     Config{Port: 8444, DisableHTTPS: false, LogSeverity: "info", MaxRequestBodyBytes: 1},
			wantErr: []string{"disableHttps must be true"},
		},
		{
			name:    "all problems joined",
			cfg:     Config{Port: 0, DisableHTTPS: false, LogSeverity: "trace", MaxRequestBodyBytes: 1},
			wantErr: []string{"port must be between", "logSeverity must be", "disableHttps must be true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			err := tc.cfg.Validate()

			// ASSERT
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			for _, want := range tc.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestValidate_ShouldRejectEmptyTokensFileWhenAuthEnabled(t *testing.T) {
	t.Parallel()

	// ARRANGE — auth on, but nowhere to read tokens from.
	cfg := Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: 1, AuthTokensFile: ""}

	// ACT
	err := cfg.Validate()

	// ASSERT — caught here, where the message can name the key.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authTokensFile must be set")
}

func TestValidate_ShouldRejectAnUnboundedRequestBody(t *testing.T) {
	t.Parallel()

	// ARRANGE — the two values an operator might expect to mean "no limit".
	for _, size := range []int{0, -1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()

			cfg := Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: size}

			// ACT
			err := cfg.Validate()

			// ASSERT — refused at startup rather than read as unlimited, which
			// would leave the server reading a body until it ran out of memory.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "maxRequestBodyBytes must be at least 1 byte")
		})
	}
}

func TestValidate_ShouldAllowEmptyTokensFileWhenAuthDisabled(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cfg := Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", MaxRequestBodyBytes: 1, DisableAuth: true}

	// ACT / ASSERT — the dev escape hatch needs no token file.
	require.NoError(t, cfg.Validate())
}
