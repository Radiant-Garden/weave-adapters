/*
Testing: key.go

Pending:

Tested:

	FlagName / EnvName / words
	  - TestFlagName_ShouldDeriveTheFlagFromTheKey: dotted camelCase to kebab-case.
	  - TestEnvName_ShouldDeriveTheVariableFromTheKey: dotted camelCase to
	    WEAVE_ADAPTER_SCREAMING_SNAKE, including the dotted-key convention this
	    milestone establishes.

	coerce
	  - TestCoerce_ShouldReadEachTypeFromEverySourceShape: native values (defaults,
	    flags, TOML) and strings (the environment) both land on the declared type.
	  - TestCoerce_ShouldRejectAnUnreadableValue: a bad value of each type errors,
	    naming the key, its environment variable, and the accepted forms.
	  - TestCoerce_ShouldReturnTheZeroValueForAnAbsentKey: a key with no default
	    that no source set resolves to its zero value rather than nil.

	Spec.index
	  - TestSpecIndex_ShouldRejectDuplicateNames: covered end-to-end in loader_test.go;
	    here for the direct message.
	  - TestSpecIndex_ShouldRejectKeysDerivingTheSameNames: two distinct key names
	    that collide only after derivation — the failure name-only checking misses.
	    Both derived names are reported, since one split produces both.
	  - TestSpecIndex_ShouldRejectAKeyClaimingTheConfigFlag: -config is the loader's
	    own, and colliding with it would panic inside flag rather than report.

	Type
	  - TestType_ShouldRenderItsName: the names that appear in operator messages.

Tested elsewhere:

	Precedence and the loader's use of all of the above: loader_test.go.
	The core keys' derived names, as a compatibility regression: config_test.go.

Declined:

	coerce's unknown-type branch and zeroOf's: unreachable while Type has four
	  values and every one is handled. They exist so a fifth type added without
	  updating both sites errors rather than silently resolving to nil.
	Non-ASCII key names: keys are our own identifiers, and words() splits on byte
	  offsets, which is exact for ASCII and undefined otherwise. Documented on Key
	  rather than defended against — there is no input path that reaches it.

Additional Remarks:

	The name derivations are the load-bearing part of this file: they replace five
	hand-written flag names and five hand-written environment variables, so an
	error in them renames the whole operator-facing surface at once. The core keys
	are pinned separately in config_test.go; the cases here cover the rule itself.
*/
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlagName_ShouldDeriveTheFlagFromTheKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want string
	}{
		{key: "port", want: "port"},
		{key: "disableHttps", want: "disable-https"},
		{key: "authTokensFile", want: "auth-tokens-file"},
		{key: "dhcp.server", want: "dhcp-server"},
		{key: "dhcp.commandTimeout", want: "dhcp-command-timeout"},
		{key: "identity.namespaceKey", want: "identity-namespace-key"},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tc.want, FlagName(tc.key))
		})
	}
}

func TestEnvName_ShouldDeriveTheVariableFromTheKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want string
	}{
		{key: "port", want: "WEAVE_ADAPTER_PORT"},
		{key: "disableHttps", want: "WEAVE_ADAPTER_DISABLE_HTTPS"},
		{key: "authTokensFile", want: "WEAVE_ADAPTER_AUTH_TOKENS_FILE"},
		{key: "dhcp.server", want: "WEAVE_ADAPTER_DHCP_SERVER"},
		// The convention this milestone writes down, per the M3 plan.
		{key: "dhcp.commandTimeout", want: "WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT"},
		{key: "scopes.defaultPageSize", want: "WEAVE_ADAPTER_SCOPES_DEFAULT_PAGE_SIZE"},
		{key: "identity.namespaceKey", want: "WEAVE_ADAPTER_IDENTITY_NAMESPACE_KEY"},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tc.want, EnvName(tc.key))
		})
	}
}

func TestCoerce_ShouldReadEachTypeFromEverySourceShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  Key
		raw  any
		want any
	}{
		{name: "string from a string", key: Key{Name: "k", Type: TypeString}, raw: "hello", want: "hello"},
		{name: "int from an int", key: Key{Name: "k", Type: TypeInt}, raw: 8444, want: 8444},
		// TOML yields int64, so this case is not hypothetical.
		{name: "int from an int64", key: Key{Name: "k", Type: TypeInt}, raw: int64(8444), want: 8444},
		{name: "int from a string", key: Key{Name: "k", Type: TypeInt}, raw: "8444", want: 8444},
		{name: "bool from a bool", key: Key{Name: "k", Type: TypeBool}, raw: true, want: true},
		{name: "bool from a word", key: Key{Name: "k", Type: TypeBool}, raw: "true", want: true},
		{name: "bool from a digit", key: Key{Name: "k", Type: TypeBool}, raw: "1", want: true},
		{
			name: "duration from a duration",
			key:  Key{Name: "k", Type: TypeDuration},
			raw:  10 * time.Second,
			want: 10 * time.Second,
		},
		{
			name: "duration from a string",
			key:  Key{Name: "k", Type: TypeDuration},
			raw:  "1m30s",
			want: 90 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			got, err := coerce(tc.key, tc.raw)

			// ASSERT
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCoerce_ShouldRejectAnUnreadableValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		key         Key
		raw         any
		wantMessage []string
	}{
		{
			name:        "an integer that is a word",
			key:         Key{Name: "port", Type: TypeInt},
			raw:         "eight",
			wantMessage: []string{"port", "WEAVE_ADAPTER_PORT", "integer", "whole number", `"eight"`},
		},
		{
			name: "a boolean that is a word ParseBool rejects",
			key:  Key{Name: "disableAuth", Type: TypeBool},
			raw:  "yes",
			wantMessage: []string{
				"disableAuth", "WEAVE_ADAPTER_DISABLE_AUTH", "boolean", "true/false", `"yes"`,
			},
		},
		{
			name: "a duration without a unit",
			key:  Key{Name: "dhcp.commandTimeout", Type: TypeDuration},
			raw:  "10",
			wantMessage: []string{
				"dhcp.commandTimeout", "WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT", "duration", `"10s"`,
			},
		},
		{
			name:        "a duration that is a word",
			key:         Key{Name: "dhcp.probeTimeout", Type: TypeDuration},
			raw:         "soon",
			wantMessage: []string{"dhcp.probeTimeout", "duration", `"soon"`},
		},
		{
			// Stringifying instead would make this "5", which then fails the
			// oneof rule describing a value the operator never wrote.
			name:        "a string key given a bare number",
			key:         Key{Name: "logSeverity", Type: TypeString},
			raw:         int64(5),
			wantMessage: []string{"logSeverity", "string", "quoted string", `"5"`},
		},
		{
			name:        "a string key given a list",
			key:         Key{Name: "authTokensFile", Type: TypeString},
			raw:         []any{"a.toml", "b.toml"},
			wantMessage: []string{"authTokensFile", "string"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := coerce(tc.key, tc.raw)

			// ASSERT — the message has to carry the key, where it most likely
			// came from, and what would have been accepted. An operator reading
			// it has no other source of that information.
			require.Error(t, err)

			for _, want := range tc.wantMessage {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestCoerce_ShouldReturnTheZeroValueForAnAbsentKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  Type
		want any
	}{
		{name: "string", typ: TypeString, want: ""},
		{name: "int", typ: TypeInt, want: 0},
		{name: "bool", typ: TypeBool, want: false},
		{name: "duration", typ: TypeDuration, want: time.Duration(0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT — a key with no default that no source set.
			got, err := coerce(Key{Name: "k", Type: tc.typ}, nil)

			// ASSERT — the typed zero, not nil: the getters type-assert, and a
			// nil would turn a missing required key into a panic rather than the
			// validation error that names it.
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSpecIndex_ShouldRejectDuplicateNames(t *testing.T) {
	t.Parallel()

	// ARRANGE
	spec := Spec{
		{Name: "dhcp.server", Type: TypeString},
		{Name: "dhcp.server", Type: TypeString},
	}

	// ACT
	_, err := spec.index()

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate config key "dhcp.server"`)
}

func TestSpecIndex_ShouldRejectKeysDerivingTheSameNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		spec        Spec
		wantMessage string
	}{
		{
			// The names differ, so a duplicate-name check passes them both. They
			// derive one environment variable, and one of them would silently
			// become unsettable from the environment.
			name: "a dot and a capital derive the same variable",
			spec: Spec{
				{Name: "dhcp.commandTimeout", Type: TypeDuration},
				{Name: "dhcp.command.timeout", Type: TypeDuration},
			},
			wantMessage: "both derive the flag -dhcp-command-timeout and WEAVE_ADAPTER_DHCP_COMMAND_TIMEOUT",
		},
		{
			name: "a segment boundary and a word boundary agree",
			spec: Spec{
				{Name: "identity.serverName", Type: TypeString},
				{Name: "identityServer.name", Type: TypeString},
			},
			wantMessage: "both derive the flag -identity-server-name and WEAVE_ADAPTER_IDENTITY_SERVER_NAME",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := tc.spec.index()

			// ASSERT — a spec is composed from core plus an adapter, which
			// cannot see each other's key lists, so this has to fail at load.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMessage)
		})
	}
}

func TestSpecIndex_ShouldRejectAKeyClaimingTheConfigFlag(t *testing.T) {
	t.Parallel()

	// ARRANGE — -config is the loader's own flag, not a registered key.
	spec := Spec{{Name: "config", Type: TypeString}}

	// ACT
	_, err := spec.index()

	// ASSERT — without this, flag.StringVar panics with "flag redefined:
	// config", which names neither the key nor the reason.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved flag -config")
}

func TestType_ShouldRenderItsName(t *testing.T) {
	t.Parallel()

	// ACT / ASSERT — these names appear verbatim in operator-facing messages.
	assert.Equal(t, "string", TypeString.String())
	assert.Equal(t, "integer", TypeInt.String())
	assert.Equal(t, "boolean", TypeBool.String())
	assert.Equal(t, "duration", TypeDuration.String())
	assert.Equal(t, "unknown", Type(99).String())
}
