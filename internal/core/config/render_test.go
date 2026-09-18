/*
Testing: render.go

Pending:

Tested:
  Render
    - TestRender_ShouldRoundTripThroughTheLoader: the assertion that matters — what is written is what resolves.
    - TestRender_ShouldWriteOnlyTheValuesItWasGiven: no key that was not provisioned reaches the file.
    - TestRender_ShouldWriteEveryTypeInItsTomlForm: string, int, bool and duration.
    - TestRender_ShouldCarryEachKeysUsageAsAComment: the file explains itself from the registration.
    - TestRender_ShouldPlaceBareKeysBeforeAnyTable: a top-level key after a [table] silently joins it.
    - TestRender_ShouldGroupDottedKeysUnderTheirTable
    - TestRender_ShouldRefuseAnUnregisteredKey: a key the loader would ignore is never written.
    - TestRender_ShouldRefuseAValueOfTheWrongType
    - TestRender_ShouldRefuseADuplicateKey
    - TestRender_ShouldReportEveryProblemAtOnce
  tomlString (through Render)
    - TestRender_ShouldWriteWindowsPathsAsLiteralStrings: a backslash must not be an escape.
    - TestRender_ShouldEscapeAValueALiteralStringCannotHold: a quote or a control character falls back to a basic string, still round-tripping.
  ResolveBytes
    - TestResolveBytes_ShouldResolveADocumentThatWasNeverWritten: the projection a provisioning run validates before writing.
    - TestResolveBytes_ShouldIgnoreTheProcessEnvironment: the answer has to be the one a service would resolve.
    - TestResolveBytes_ShouldReportADocumentThatDoesNotParse
  ValueOf
    - TestValueOf_ShouldReturnAResolvedValueWithoutPanicking: the typed getters panic, and a caller comparing provisioned values cannot.
    - TestValueOf_ShouldReportAnUnregisteredKey
  RegisterFlags
    - TestRegisterFlags_ShouldDeriveOneFlagPerSettableKey: no second vocabulary.
    - TestRegisterFlags_ShouldReportOnlyTheFlagsThatWereSet: an unset flag's zero must never be provisioned.
    - TestRegisterFlags_ShouldNotOfferAFlagForANoFlagKey: NoFlag is inherited, not restated.
    - TestRegisterFlags_ShouldReportInSpecOrderRatherThanTypedOrder

Tested elsewhere:
  What the generated file is FOR — the provisioning run that writes it, and the
  decision to validate before writing — is internal/core/setup's.

Declined:
  Asserting the exact bytes of the header, or the column a comment wraps at.
  Both are presentation, and a test that pinned them would fail on a reworded
  sentence while proving nothing about whether the file loads.

Additional Remarks:
  The round-trip test is the one to keep honest. Every other test here asserts
  something about the TEXT, and text assertions pass happily for a file the
  loader cannot read — which is the failure that actually costs something,
  because the file is written with identity.namespaceKey already in it and a
  quoting mistake is discovered only after the single copy of that key is on
  disk.

  The Windows-path case is not hypothetical. Every path this renderer writes on
  a real host is a Windows path, and C:\temp\new as a TOML basic string carries
  a literal tab and newline into the value — which the loader accepts, so the
  service starts and writes its log somewhere nobody can find.
*/

package config

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderSpec is a spec covering all four types, both key shapes, and a key
// with no Usage.
func renderSpec() Spec {
	return Spec{
		{Name: "logFile", Type: TypeString, Path: FilePath, Usage: "where the log stream is written"},
		{Name: "port", Type: TypeInt, Default: 8444, Usage: "the TCP port the HTTP server listens on"},
		{Name: "disableAuth", Type: TypeBool, Usage: "turn off bearer authentication (development only)"},
		{Name: "dhcp.commandTimeout", Type: TypeDuration, Usage: "per-invocation backend timeout"},
		{Name: "identity.namespaceKey", Type: TypeString, NoFlag: true, Usage: "HMAC key for ID derivation"},
		{Name: "identity.serverName", Type: TypeString, Usage: "canonical server identity"},
		{Name: "quiet", Type: TypeString},
	}
}

// renderAndLoad renders, writes and resolves, which is the whole contract.
func renderAndLoad(t *testing.T, values []Provisioned) *Values {
	t.Helper()

	data, err := Render(renderSpec(), values)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	loaded, err := LoadWithoutEnvironment(renderSpec(), []string{"--config", path})
	require.NoError(t, err, "the generated file did not load:\n%s", data)

	return loaded
}

func TestRender_ShouldRoundTripThroughTheLoader(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values := []Provisioned{
		{Key: "logFile", Value: `C:\ProgramData\weave-adapters\adapter.log`},
		{Key: "port", Value: 9001},
		{Key: "disableAuth", Value: true},
		{Key: "dhcp.commandTimeout", Value: 12 * time.Second},
		{Key: "identity.namespaceKey", Value: "a-provisioned-namespace-key", Secret: true},
		{Key: "identity.serverName", Value: "dhcp01.example.test"},
	}

	// ACT
	loaded := renderAndLoad(t, values)

	// ASSERT
	// The only assertion that proves the renderer works: text checks pass
	// happily for a file the loader cannot read.
	assert.Equal(t, `C:\ProgramData\weave-adapters\adapter.log`, loaded.String("logFile"))
	assert.Equal(t, 9001, loaded.Int("port"))
	assert.True(t, loaded.Bool("disableAuth"))
	assert.Equal(t, 12*time.Second, loaded.Duration("dhcp.commandTimeout"))
	assert.Equal(t, "a-provisioned-namespace-key", loaded.String("identity.namespaceKey"))
	assert.Equal(t, "dhcp01.example.test", loaded.String("identity.serverName"))
}

func TestRender_ShouldWriteOnlyTheValuesItWasGiven(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values := []Provisioned{{Key: "identity.serverName", Value: "dhcp01.example.test"}}

	// ACT
	data, err := Render(renderSpec(), values)

	// ASSERT
	// Rendering the whole spec would freeze today's defaults into every
	// provisioned host — a file written with one timeout keeps it after the
	// default is raised, and this repo has raised one.
	require.NoError(t, err)

	rendered := string(data)
	assert.Contains(t, rendered, "serverName")

	for _, absent := range []string{"port", "disableAuth", "commandTimeout", "logFile"} {
		assert.NotContains(t, rendered, absent+" =", "%s was written without being provisioned", absent)
	}

	// And the key that was not written still resolves, to its default.
	loaded := renderAndLoad(t, values)
	assert.Equal(t, 8444, loaded.Int("port"))
}

func TestRender_ShouldWriteEveryTypeInItsTomlForm(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	data, err := Render(renderSpec(), []Provisioned{
		{Key: "port", Value: 9001},
		{Key: "disableAuth", Value: false},
		{Key: "dhcp.commandTimeout", Value: 90 * time.Second},
	})

	// ASSERT
	require.NoError(t, err)

	rendered := string(data)
	assert.Contains(t, rendered, "port = 9001", "an int must not be quoted")
	assert.Contains(t, rendered, "disableAuth = false")
	// A duration is only ever parsed from a string in TOML.
	assert.Contains(t, rendered, "commandTimeout = '1m30s'")
}

func TestRender_ShouldWriteWindowsPathsAsLiteralStrings(t *testing.T) {
	t.Parallel()

	// ARRANGE — \t and \n are what a basic string would mangle.
	path := `C:\temp\new\adapter.log`

	// ACT
	data, err := Render(renderSpec(), []Provisioned{{Key: "logFile", Value: path}})
	require.NoError(t, err)

	// ASSERT
	assert.Contains(t, string(data), "logFile = '"+path+"'",
		"a Windows path must be a literal string, where a backslash is not an escape")

	loaded := renderAndLoad(t, []Provisioned{{Key: "logFile", Value: path}})
	assert.Equal(t, path, loaded.String("logFile"),
		"the path came back mangled, which is how a service ends up logging somewhere nobody can find")
}

func TestRender_ShouldEscapeAValueALiteralStringCannotHold(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"should escape a single quote":      `it's-a-key-with-a-quote`,
		"should escape a backslash too":     `a'key\with\both`,
		"should escape a control character": "a-key-with-a\ttab",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			values := []Provisioned{{Key: "identity.namespaceKey", Value: value}}

			// ACT
			loaded := renderAndLoad(t, values)

			// ASSERT
			// A literal string cannot carry either, so this falls back to a
			// basic string — and the fallback has to round-trip, or the file
			// is unreadable with the only copy of the key already in it.
			assert.Equal(t, value, loaded.String("identity.namespaceKey"))
		})
	}
}

func TestRender_ShouldCarryEachKeysUsageAsAComment(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	data, err := Render(renderSpec(), []Provisioned{
		{Key: "port", Value: 9001},
		{Key: "quiet", Value: "no usage on this key"},
	})

	// ASSERT
	// From the registration rather than a second copy of the documentation,
	// so the file cannot drift from what the key actually does.
	require.NoError(t, err)
	assert.Contains(t, string(data), "# the TCP port the HTTP server listens on")

	// A key with no Usage still renders, without an empty comment marker.
	assert.Contains(t, string(data), "quiet = 'no usage on this key'")
	assert.NotContains(t, string(data), "#\nquiet")
}

func TestRender_ShouldPlaceBareKeysBeforeAnyTable(t *testing.T) {
	t.Parallel()

	// ARRANGE — provisioned table-first, which is the order that would break.
	values := []Provisioned{
		{Key: "identity.serverName", Value: "dhcp01.example.test"},
		{Key: "logFile", Value: `C:\logs\adapter.log`},
	}

	// ACT
	data, err := Render(renderSpec(), values)
	require.NoError(t, err)

	// ASSERT
	// In TOML a bare key written after a [table] header belongs to that table,
	// so logFile emitted late would become identity.logFile and never be read
	// — with nothing reporting it.
	rendered := string(data)
	assert.Less(t, indexOf(t, rendered, "logFile ="), indexOf(t, rendered, "[identity]"))

	loaded := renderAndLoad(t, values)
	assert.Equal(t, `C:\logs\adapter.log`, loaded.String("logFile"))
}

func TestRender_ShouldGroupDottedKeysUnderTheirTable(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	data, err := Render(renderSpec(), []Provisioned{
		{Key: "identity.serverName", Value: "dhcp01.example.test"},
		{Key: "identity.namespaceKey", Value: "a-provisioned-namespace-key"},
		{Key: "dhcp.commandTimeout", Value: time.Second},
	})

	// ASSERT
	require.NoError(t, err)

	rendered := string(data)
	assert.Contains(t, rendered, "[identity]")
	assert.Contains(t, rendered, "[dhcp]")
	// One header per table, however many keys it holds.
	assert.Equal(t, 1, countOf(rendered, "[identity]"))
}

func TestRender_ShouldRefuseAnUnregisteredKey(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	data, err := Render(renderSpec(), []Provisioned{{Key: "notAKey", Value: "x"}})

	// ASSERT
	// Written, parsed, and ignored by the loader is the alternative: a config
	// that visibly says one thing and does another.
	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "not a registered key")
}

func TestRender_ShouldRefuseAValueOfTheWrongType(t *testing.T) {
	t.Parallel()

	tests := map[string]Provisioned{
		"should refuse a string for an int":     {Key: "port", Value: "9001"},
		"should refuse an int for a string":     {Key: "logFile", Value: 7},
		"should refuse a string for a bool":     {Key: "disableAuth", Value: "true"},
		"should refuse a string for a duration": {Key: "dhcp.commandTimeout", Value: "10s"},
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT
			data, err := Render(renderSpec(), []Provisioned{value})

			// ASSERT
			// Refused at render rather than at the next start: the file is
			// written with the namespace key already in it, so "discovered
			// later" means discovered after the only copy is on disk.
			require.Error(t, err)
			assert.Nil(t, data)
			assert.Contains(t, err.Error(), value.Key)
		})
	}
}

func TestRender_ShouldRefuseADuplicateKey(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	_, err := Render(renderSpec(), []Provisioned{
		{Key: "port", Value: 9001},
		{Key: "port", Value: 9002},
	})

	// ASSERT
	// TOML would take one of them and the operator could not tell which.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "twice")
}

func TestRender_ShouldReportEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	_, err := Render(renderSpec(), []Provisioned{
		{Key: "notAKey", Value: "x"},
		{Key: "port", Value: "not an int"},
	})

	// ASSERT
	// Joined for the same reason the configuration errors are: fixing one and
	// being told about the next is the version that wastes an afternoon.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notAKey")
	assert.Contains(t, err.Error(), "port")
}

// indexOf returns the position of needle, failing the test when it is absent
// so a missing string does not read as "position -1, which is less".
func indexOf(t *testing.T, haystack, needle string) int {
	t.Helper()

	i := strings.Index(haystack, needle)
	require.GreaterOrEqual(t, i, 0, "%q is not in the rendered file:\n%s", needle, haystack)

	return i
}

// countOf counts non-overlapping occurrences.
func countOf(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

func TestResolveBytes_ShouldResolveADocumentThatWasNeverWritten(t *testing.T) {
	t.Parallel()

	// ARRANGE
	data, err := Render(renderSpec(), []Provisioned{
		{Key: "port", Value: 9001},
		{Key: "identity.serverName", Value: "dhcp01.example.test"},
	})
	require.NoError(t, err)

	// ACT
	values, err := ResolveBytes(renderSpec(), `C:\destined\config.toml`, data)

	// ASSERT
	// This is what lets a provisioning run validate the file it is ABOUT to
	// write. Rendering, writing and then validating leaves a failed run with a
	// fresh identity.namespaceKey on disk that nobody asked for — and on the
	// next run that key is indistinguishable from a provisioned one, so the
	// mistake is not recoverable by re-running.
	require.NoError(t, err)
	assert.Equal(t, 9001, values.Int("port"))
	assert.Equal(t, "dhcp01.example.test", values.String("identity.serverName"))

	// Defaults still apply, so the projection answers what the service would.
	assert.Empty(t, values.String("logFile"))

	// And the destination is recorded even though nothing is there yet.
	assert.Equal(t, `C:\destined\config.toml`, values.ConfigPath())
}

// Not parallel: t.Setenv mutates the process environment, which is exactly
// what this test is about. The loader's own environment tests stay parallel by
// injecting EnvironFunc; ResolveBytes takes no such seam because it never
// consults the environment at all, and proving that needs a real variable.
func TestResolveBytes_ShouldIgnoreTheProcessEnvironment(t *testing.T) {
	// ARRANGE
	t.Setenv("WEAVE_ADAPTER_PORT", "7777")

	data, err := Render(renderSpec(), []Provisioned{{Key: "port", Value: 9001}})
	require.NoError(t, err)

	// ACT
	values, err := ResolveBytes(renderSpec(), "config.toml", data)

	// ASSERT
	// The answer has to be the one a SERVICE would resolve. The process asking
	// is an elevated operator's shell, and a value exported there would make
	// every check pass and then be absent at every boot.
	require.NoError(t, err)
	assert.Equal(t, 9001, values.Int("port"))
}

func TestResolveBytes_ShouldReportADocumentThatDoesNotParse(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	_, err := ResolveBytes(renderSpec(), "config.toml", []byte("not = = toml"))

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing the rendered configuration")
}

func TestValueOf_ShouldReturnAResolvedValueWithoutPanicking(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values := renderAndLoad(t, []Provisioned{
		{Key: "port", Value: 9001},
		{Key: "logFile", Value: `C:\logs\adapter.log`},
	})

	// ACT / ASSERT
	// The typed getters panic, because reading the wrong type is a wiring
	// mistake in a binary that knows its own keys. A caller comparing a
	// PROVISIONED value against a resolved one does not know the type ahead of
	// time — it holds whatever the operator supplied — so it needs an accessor
	// that reports rather than crashes.
	port, err := ValueOf(values, "port")
	require.NoError(t, err)
	assert.Equal(t, 9001, port)

	logFile, err := ValueOf(values, "logFile")
	require.NoError(t, err)
	assert.Equal(t, `C:\logs\adapter.log`, logFile)
}

func TestValueOf_ShouldReportAnUnregisteredKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	values := renderAndLoad(t, []Provisioned{{Key: "port", Value: 9001}})

	// ACT
	_, err := ValueOf(values, "notAKey")

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered")
}

func TestRegisterFlags_ShouldDeriveOneFlagPerSettableKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	provisionedFrom, err := RegisterFlags(fs, renderSpec())
	require.NoError(t, err)

	// ACT
	require.NoError(t, fs.Parse([]string{
		"-" + FlagName("port"), "9001",
		"-" + FlagName("identity.serverName"), "dhcp01.example.test",
		"-" + FlagName("dhcp.commandTimeout"), "12s",
		"-" + FlagName("disableAuth"),
	}))

	// ASSERT
	// Derived names, so a command layered on this has no vocabulary of its
	// own: --identity-server-name exists because identity.serverName is a key.
	provisioned := provisionedFrom()

	assert.Contains(t, provisioned, Provisioned{Key: "port", Value: 9001})
	assert.Contains(t, provisioned, Provisioned{Key: "identity.serverName", Value: "dhcp01.example.test"})
	assert.Contains(t, provisioned, Provisioned{Key: "dhcp.commandTimeout", Value: 12 * time.Second})
	assert.Contains(t, provisioned, Provisioned{Key: "disableAuth", Value: true})
}

func TestRegisterFlags_ShouldReportOnlyTheFlagsThatWereSet(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	provisionedFrom, err := RegisterFlags(fs, renderSpec())
	require.NoError(t, err)

	// ACT
	require.NoError(t, fs.Parse([]string{"-" + FlagName("identity.serverName"), "dhcp01.example.test"}))

	// ASSERT
	// An unset flag's zero value must never become a provisioned value:
	// writing port = 0 into a generated config because nobody passed --port is
	// the failure this guards, and the file would then pin it forever.
	provisioned := provisionedFrom()

	require.Len(t, provisioned, 1)
	assert.Equal(t, "identity.serverName", provisioned[0].Key)
}

func TestRegisterFlags_ShouldNotOfferAFlagForANoFlagKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	_, err := RegisterFlags(fs, renderSpec())
	require.NoError(t, err)

	// ACT / ASSERT
	// Inherited from the key's registration rather than restated by every
	// command: a flag value is an argv entry any local user can read from a
	// process listing, and identity.namespaceKey is backup-critical.
	assert.Nil(t, fs.Lookup(FlagName("identity.namespaceKey")))
	assert.NotNil(t, fs.Lookup(FlagName("identity.serverName")))

	require.Error(t, fs.Parse([]string{"-" + FlagName("identity.namespaceKey"), "a-key"}))
}

func TestRegisterFlags_ShouldReportInSpecOrderRatherThanTypedOrder(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	provisionedFrom, err := RegisterFlags(fs, renderSpec())
	require.NoError(t, err)

	// ACT — typed in the reverse of the spec's order.
	require.NoError(t, fs.Parse([]string{
		"-" + FlagName("identity.serverName"), "dhcp01.example.test",
		"-" + FlagName("port"), "9001",
		"-" + FlagName("logFile"), `C:\logs\adapter.log`,
	}))

	// ASSERT
	// So a generated file reads the same however the command line was written
	// — two hosts provisioned with the same values get the same file.
	provisioned := provisionedFrom()

	keys := make([]string, 0, len(provisioned))
	for _, v := range provisioned {
		keys = append(keys, v.Key)
	}

	assert.Equal(t, []string{"logFile", "port", "identity.serverName"}, keys)
}
