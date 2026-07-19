/*
Testing: loader.go

Pending:

Tested:

	load
	  - TestLoad_ShouldReturnDefaultsWhenNoSourcesSet: registered defaults apply.
	  - TestLoad_ShouldApplyPrecedence: flags > env > file > defaults ordering.
	  - TestLoad_ShouldOverrideFileWithEnv: env wins over a TOML file; unset file keys survive.
	  - TestLoad_ShouldReadEveryRegisteredType: a spec covering all four types round-trips
	    from each source.
	  - TestLoad_ShouldErrorWhenConfigFileMissing: a missing --config path is an error.
	  - TestLoad_ShouldErrorWhenConfigFileMalformed: an existing but invalid TOML file errors.
	  - TestLoad_ShouldErrorWhenFlagInvalid: a flag-parse failure propagates from parseFlags.
	  - TestLoad_ShouldRejectADuplicateKey: two registrations of one name are refused.
	  - TestLoad_ShouldJoinEveryTypeError: all unreadable values are reported in one run.
	  - TestLoad_ShouldReadBooleanEnvVars: booleans apply in every spelling ParseBool accepts.
	  - TestLoad_ShouldRejectANonBooleanEnvVar: an unreadable boolean is an error, not a silent false.
	  - TestLoad_ShouldIgnoreUnregisteredEnvVars: a WEAVE_ADAPTER_* name nothing registered is not a key.

	Values
	  - TestValues_ShouldPanicOnAnUnregisteredKey: a wiring mistake fails loudly at startup.
	  - TestValues_ShouldPanicOnATypeMismatch: reading a key as the wrong type likewise.

Tested elsewhere:

	Name derivation, per-type coercion and its messages: key_test.go.
	Core's struct construction and validation: config_test.go.

Declined:

	Fuzzing load: the TOML/env parsing is koanf's (pelletier) provider — we don't
	  fuzz what we don't own — and the transform is a map lookup.
	parseFlags' unknown-type branch and resolve's: both are unreachable while Type
	  has four values and every one is handled; they exist so a fifth added without
	  updating both sites errors rather than silently dropping the key.

Additional Remarks:

	Tests drive the unexported load(spec, args, environ) so environment precedence
	can be exercised through an injected EnvironFunc, keeping every test
	parallel-safe. Most use CoreKeys(); the type-coverage tests declare a local
	spec, which is also the first proof that a non-core spec works at all.
*/
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// environ returns an EnvironFunc yielding the given KEY=VALUE pairs, so tests
// inject environment variables without touching the real OS environment.
func environ(pairs ...string) func() []string {
	return func() []string { return pairs }
}

// typedSpec is a spec exercising all four value types, including the dotted
// keys and the duration type that core itself does not yet use.
func typedSpec() Spec {
	return Spec{
		{Name: "demo.name", Type: TypeString, Default: "unset", Usage: "a string"},
		{Name: "demo.count", Type: TypeInt, Default: 3, Usage: "an integer"},
		{Name: "demo.enabled", Type: TypeBool, Default: false, Usage: "a boolean"},
		{Name: "demo.commandTimeout", Type: TypeDuration, Default: 10 * time.Second, Usage: "a duration"},
	}
}

func TestLoad_ShouldReturnDefaultsWhenNoSourcesSet(t *testing.T) {
	t.Parallel()

	// ACT
	values, err := load(CoreKeys(), nil, environ())

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, defaultPort, values.Int(KeyPort))
	assert.True(t, values.Bool(KeyDisableHTTPS))
	assert.Equal(t, defaultLogSeverity, values.String(KeyLogSeverity))
	assert.Equal(t, DefaultAuthTokensFile, values.String(KeyAuthTokensFile))
	assert.False(t, values.Bool(KeyDisableAuth))
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
			values, err := load(CoreKeys(), tc.args, environ(tc.env...))

			// ASSERT
			require.NoError(t, err)
			assert.Equal(t, tc.wantPort, values.Int(KeyPort))
			assert.Equal(t, tc.wantSev, values.String(KeyLogSeverity))
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
	fromFile, err := load(CoreKeys(), []string{"-config", path}, environ())

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 9000, fromFile.Int(KeyPort))
	assert.Equal(t, "debug", fromFile.String(KeyLogSeverity))

	// ACT — env overrides the file's port; the file's logSeverity survives
	withEnv, err := load(CoreKeys(), []string{"-config", path}, environ("WEAVE_ADAPTER_PORT=9500"))

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 9500, withEnv.Int(KeyPort))
	assert.Equal(t, "debug", withEnv.String(KeyLogSeverity))
}

func TestLoad_ShouldReadEveryRegisteredType(t *testing.T) {
	t.Parallel()

	// ARRANGE — a TOML file carrying every type, with the duration as the string
	// TOML can express.
	path := filepath.Join(t.TempDir(), "typed.toml")
	body := "[demo]\nname = \"from-file\"\ncount = 7\nenabled = true\ncommandTimeout = \"90s\"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	tests := []struct {
		name        string
		args        []string
		env         []string
		wantName    string
		wantCount   int
		wantEnabled bool
		wantTimeout time.Duration
	}{
		{
			name:        "defaults",
			wantName:    "unset",
			wantCount:   3,
			wantEnabled: false,
			wantTimeout: 10 * time.Second,
		},
		{
			name:        "from the file",
			args:        []string{"-config", path},
			wantName:    "from-file",
			wantCount:   7,
			wantEnabled: true,
			wantTimeout: 90 * time.Second,
		},
		{
			name: "from the environment",
			env: []string{
				"WEAVE_ADAPTER_DEMO_NAME=from-env",
				"WEAVE_ADAPTER_DEMO_COUNT=11",
				"WEAVE_ADAPTER_DEMO_ENABLED=1",
				"WEAVE_ADAPTER_DEMO_COMMAND_TIMEOUT=1m30s",
			},
			wantName:    "from-env",
			wantCount:   11,
			wantEnabled: true,
			wantTimeout: 90 * time.Second,
		},
		{
			name: "from flags, over everything",
			args: []string{
				"-config", path,
				"-demo-name", "from-flag",
				"-demo-count", "13",
				"-demo-enabled=false",
				"-demo-command-timeout", "2m",
			},
			env:         []string{"WEAVE_ADAPTER_DEMO_NAME=from-env"},
			wantName:    "from-flag",
			wantCount:   13,
			wantEnabled: false,
			wantTimeout: 2 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			values, err := load(typedSpec(), tc.args, environ(tc.env...))

			// ASSERT
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, values.String("demo.name"))
			assert.Equal(t, tc.wantCount, values.Int("demo.count"))
			assert.Equal(t, tc.wantEnabled, values.Bool("demo.enabled"))
			assert.Equal(t, tc.wantTimeout, values.Duration("demo.commandTimeout"))
		})
	}
}

func TestLoad_ShouldReadBooleanEnvVars(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — DISABLE_AUTH had no env-level coverage at all, which is
	// how a key spelled in several places goes unnoticed when one is missed. Its
	// default is false, so reading true proves the variable was really applied.
	spelled, err := load(CoreKeys(), nil, environ("WEAVE_ADAPTER_DISABLE_AUTH=true"))

	// ASSERT
	require.NoError(t, err)
	assert.True(t, spelled.Bool(KeyDisableAuth))

	// ACT — every spelling ParseBool accepts, not just the word.
	numeric, err := load(CoreKeys(), nil, environ("WEAVE_ADAPTER_DISABLE_AUTH=1"))

	// ASSERT
	require.NoError(t, err)
	assert.True(t, numeric.Bool(KeyDisableAuth))
}

func TestLoad_ShouldRejectANonBooleanEnvVar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		environ string
	}{
		{name: "should reject a word that is not a boolean", environ: "WEAVE_ADAPTER_DISABLE_AUTH=yes"},
		{name: "should reject an empty value", environ: "WEAVE_ADAPTER_DISABLE_AUTH="},
		{name: "should reject it for every boolean key", environ: "WEAVE_ADAPTER_DISABLE_HTTPS=on"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := load(CoreKeys(), nil, environ(tt.environ))

			// ASSERT — koanf's k.Bool would answer false and discard the parse
			// failure. That fails safe, but an operator who asked for something
			// and was told nothing cannot discover the setting was ignored.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not a valid boolean")
		})
	}
}

func TestLoad_ShouldIgnoreUnregisteredEnvVars(t *testing.T) {
	t.Parallel()

	// ACT — a prefixed variable no key claims. It must not become a key, and
	// must not error: the environment is shared, and an adapter's own variables
	// are unregistered from another binary's point of view.
	values, err := load(CoreKeys(), nil, environ("WEAVE_ADAPTER_NOT_A_KEY=whatever"))

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, defaultPort, values.Int(KeyPort))
}

func TestLoad_ShouldJoinEveryTypeError(t *testing.T) {
	t.Parallel()

	// ACT — three unreadable values at once.
	_, err := load(typedSpec(), nil, environ(
		"WEAVE_ADAPTER_DEMO_COUNT=eleven",
		"WEAVE_ADAPTER_DEMO_ENABLED=maybe",
		"WEAVE_ADAPTER_DEMO_COMMAND_TIMEOUT=soon",
	))

	// ASSERT — an operator fixing configuration sees all of it in one run.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demo.count")
	assert.Contains(t, err.Error(), "demo.enabled")
	assert.Contains(t, err.Error(), "demo.commandTimeout")
}

func TestLoad_ShouldRejectADuplicateKey(t *testing.T) {
	t.Parallel()

	// ARRANGE — the shape a spec composed from core plus an adapter takes when
	// the adapter picks a name core already owns.
	spec := append(CoreKeys(), Key{Name: KeyPort, Type: TypeInt, Default: 1})

	// ACT
	_, err := load(spec, nil, environ())

	// ASSERT — resolving by declaration order would be a silent surprise.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate config key")
}

func TestLoad_ShouldErrorWhenConfigFileMissing(t *testing.T) {
	t.Parallel()

	// ACT
	_, err := load(CoreKeys(), []string{"-config", "/no/such/file.toml"}, environ())

	// ASSERT
	require.Error(t, err)
}

func TestLoad_ShouldErrorWhenConfigFileMalformed(t *testing.T) {
	t.Parallel()

	// ARRANGE — an existing file whose contents are not valid TOML.
	path := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("@@@ not toml @@@\n"), 0o600))

	// ACT
	_, err := load(CoreKeys(), []string{"-config", path}, environ())

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config file")
}

func TestLoad_ShouldErrorWhenFlagInvalid(t *testing.T) {
	t.Parallel()

	// ACT — a non-numeric -port makes flag.Parse fail inside parseFlags.
	_, err := load(CoreKeys(), []string{"-port", "notanumber"}, environ())

	// ASSERT
	require.Error(t, err)
}

func TestValues_ShouldPanicOnAnUnregisteredKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values, err := load(CoreKeys(), nil, environ())
	require.NoError(t, err)

	// ACT / ASSERT — no operator input causes this, so there is nothing to
	// handle: it is a wiring mistake, and startup wiring surfaces it on the
	// first run.
	assert.PanicsWithValue(t, `config: key "dhcp.server" is not registered`, func() {
		values.String("dhcp.server")
	})
}

func TestValues_ShouldPanicOnATypeMismatch(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values, err := load(CoreKeys(), nil, environ())
	require.NoError(t, err)

	// ACT / ASSERT
	assert.PanicsWithValue(t, `config: key "port" is of type integer, read as string`, func() {
		values.String(KeyPort)
	})
}
