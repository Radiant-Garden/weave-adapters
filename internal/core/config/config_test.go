/*
Testing: config.go

Pending:

Tested:
  Load
    - TestLoad_ShouldReturnDefaultsWhenNoSourcesSet: defaults apply when nothing is set.
    - TestLoad_ShouldApplyPrecedence: flags > env > defaults ordering.
    - TestLoad_ShouldOverrideFileWithEnv: env wins over a TOML file; unset file keys survive.
    - TestLoad_ShouldErrorWhenConfigFileMissing: a missing --config path is an error.
    - TestLoad_ShouldErrorWhenConfigFileMalformed: an existing but invalid TOML file errors.
    - TestLoad_ShouldErrorWhenFlagInvalid: a flag-parse failure propagates from parseFlags.
    - TestLoad_ShouldRejectWhenHTTPSEnabled: disableHttps=false fails validation (also covers env string->bool).

  Validate / fieldError
    - TestValidate_ShouldReportProblems: valid and invalid configs, including all errors joined.
    - TestLoad_ShouldDefaultAuthSettings: auth is on by default, pointing at tokens.toml.
    - TestLoad_ShouldResolveAuthTokensFilePrecedence: flag beats env for the token path.
    - TestValidate_ShouldRejectEmptyTokensFileWhenAuthEnabled: no silent empty allow-list.
    - TestValidate_ShouldAllowEmptyTokensFileWhenAuthDisabled: the dev hatch needs no file.

Tested elsewhere:

Declined:
  Non-numeric WEAVE_ADAPTER_PORT coerced to 0: koanf's k.Int swallows the parse
    error, so Validate reports "got 0" rather than naming the bad input. Left as
    is — surfacing the raw value means bypassing the typed getter with a manual
    pre-parse for marginal message quality, and the error still points at the port.
  Fuzzing Load: the TOML/env parsing is koanf's (pelletier) provider — we don't
    fuzz what we don't own — and our transform/validation is trivial.
  Validate's non-ValidationErrors branch and fieldError's default case: defensive
    and unreachable today (only *Config is validated; only Port/LogSeverity carry
    tags), so they are documented in-code rather than tested.

Additional Remarks:
  Tests drive the unexported load(args, environ) so environment precedence can be
  exercised through an injected EnvironFunc, keeping every test parallel-safe.
*/

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// environ returns an EnvironFunc yielding the given KEY=VALUE pairs, so tests
// inject environment variables without touching the real OS environment.
func environ(pairs ...string) func() []string {
	return func() []string { return pairs }
}

func TestLoad_ShouldReturnDefaultsWhenNoSourcesSet(t *testing.T) {
	t.Parallel()

	// ACT
	cfg, err := load(nil, environ())

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, defaultPort, cfg.Port)
	assert.True(t, cfg.DisableHTTPS)
	assert.Equal(t, defaultLogSeverity, cfg.LogSeverity)
}

func TestLoad_ShouldApplyPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		env      []string
		wantPort int
		wantSev  string
	}{
		{
			name:     "defaults when nothing set",
			wantPort: defaultPort,
			wantSev:  defaultLogSeverity,
		},
		{
			name:     "env over defaults",
			env:      []string{"WEAVE_ADAPTER_PORT=9100", "WEAVE_ADAPTER_LOG_SEVERITY=warn"},
			wantPort: 9100,
			wantSev:  "warn",
		},
		{
			name:     "flags over env",
			args:     []string{"-port", "9200"},
			env:      []string{"WEAVE_ADAPTER_PORT=9100"},
			wantPort: 9200,
			wantSev:  defaultLogSeverity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			cfg, err := load(tc.args, environ(tc.env...))

			// ASSERT
			require.NoError(t, err)
			assert.Equal(t, tc.wantPort, cfg.Port)
			assert.Equal(t, tc.wantSev, cfg.LogSeverity)
		})
	}
}

func TestLoad_ShouldOverrideFileWithEnv(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "port = 9000\ndisableHttps = true\nlogSeverity = \"debug\"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	// ACT — file supplies all values
	fromFile, err := load([]string{"-config", path}, environ())

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 9000, fromFile.Port)
	assert.Equal(t, "debug", fromFile.LogSeverity)

	// ACT — env overrides the file's port; the file's logSeverity survives
	withEnv, err := load([]string{"-config", path}, environ("WEAVE_ADAPTER_PORT=9500"))

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 9500, withEnv.Port)
	assert.Equal(t, "debug", withEnv.LogSeverity)
}

func TestLoad_ShouldErrorWhenConfigFileMissing(t *testing.T) {
	t.Parallel()

	// ACT
	_, err := load([]string{"-config", "/no/such/file.toml"}, environ())

	// ASSERT
	require.Error(t, err)
}

func TestLoad_ShouldErrorWhenConfigFileMalformed(t *testing.T) {
	t.Parallel()

	// ARRANGE — an existing file whose contents are not valid TOML.
	path := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("@@@ not toml @@@\n"), 0o600))

	// ACT
	_, err := load([]string{"-config", path}, environ())

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config file")
}

func TestLoad_ShouldErrorWhenFlagInvalid(t *testing.T) {
	t.Parallel()

	// ACT — a non-numeric -port makes flag.Parse fail inside parseFlags.
	_, err := load([]string{"-port", "notanumber"}, environ())

	// ASSERT
	require.Error(t, err)
}

func TestLoad_ShouldRejectWhenHTTPSEnabled(t *testing.T) {
	t.Parallel()

	// ACT — also exercises env string->bool parsing ("false" -> false)
	_, err := load(nil, environ("WEAVE_ADAPTER_DISABLE_HTTPS=false"))

	// ASSERT
	require.Error(t, err)
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
			cfg:  Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", AuthTokensFile: "tokens.toml"},
		},
		{
			name:    "port too low",
			cfg:     Config{Port: 0, DisableHTTPS: true, LogSeverity: "info", AuthTokensFile: "tokens.toml"},
			wantErr: []string{"port must be between"},
		},
		{
			name:    "port too high",
			cfg:     Config{Port: 70000, DisableHTTPS: true, LogSeverity: "info"},
			wantErr: []string{"port must be between"},
		},
		{
			name:    "unknown severity",
			cfg:     Config{Port: 8444, DisableHTTPS: true, LogSeverity: "trace"},
			wantErr: []string{"logSeverity must be"},
		},
		{
			name:    "https enabled",
			cfg:     Config{Port: 8444, DisableHTTPS: false, LogSeverity: "info"},
			wantErr: []string{"disableHttps must be true"},
		},
		{
			name:    "all problems joined",
			cfg:     Config{Port: 0, DisableHTTPS: false, LogSeverity: "trace"},
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

func TestLoad_ShouldDefaultAuthSettings(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	cfg, err := load(nil, func() []string { return nil })

	// ASSERT — auth is on by default; the token file is the one the CLI writes.
	require.NoError(t, err)
	assert.Equal(t, defaultAuthTokensFile, cfg.AuthTokensFile)
	assert.False(t, cfg.DisableAuth)
}

func TestLoad_ShouldResolveAuthTokensFilePrecedence(t *testing.T) {
	t.Parallel()

	// ARRANGE — env set, flag overrides it.
	environ := func() []string { return []string{envAuthTokensFile + "=/from/env.toml"} }

	// ACT
	fromEnv, err := load(nil, environ)
	require.NoError(t, err)

	fromFlag, err := load([]string{"--auth-tokens-file", "/from/flag.toml"}, environ)
	require.NoError(t, err)

	// ASSERT
	assert.Equal(t, "/from/env.toml", fromEnv.AuthTokensFile)
	assert.Equal(t, "/from/flag.toml", fromFlag.AuthTokensFile)
}

func TestValidate_ShouldRejectEmptyTokensFileWhenAuthEnabled(t *testing.T) {
	t.Parallel()

	// ARRANGE — auth on, but nowhere to read tokens from.
	cfg := Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", AuthTokensFile: ""}

	// ACT
	err := cfg.Validate()

	// ASSERT — caught here, where the message can name the key.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authTokensFile must be set")
}

func TestValidate_ShouldAllowEmptyTokensFileWhenAuthDisabled(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cfg := Config{Port: 8444, DisableHTTPS: true, LogSeverity: "info", DisableAuth: true}

	// ACT / ASSERT — the dev escape hatch needs no token file.
	require.NoError(t, cfg.Validate())
}
