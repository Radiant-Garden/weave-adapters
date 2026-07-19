/*
Testing: client.go

Pending:

	The non-ASCII fixture is constructed, not captured. The plan requires a real
	  capture from the WS2022 host (a scope with an umlaut in name and
	  description) once one is reachable — the point of that fixture is to prove
	  the [Console]::OutputEncoding line works against a real OEM-code-page host,
	  and a hand-written file cannot prove that. It still guards the Go decode
	  side, which is why it is here rather than deferred entirely.
	Verifying on the host that stderr is silent across a successful run, which is
	  what would let stderr's presence become a typed error rather than context
	  attached to other errors. Until then runError treats it as evidence.

Tested:

	ListScopes
	  - TestListScopes_ShouldDecodeTheCapturedFixture: the canonical host capture
	    round-trips into the ten-field model, byte-for-byte as PS 5.1 emitted it.
	  - TestListScopes_ShouldDecodeASingleElementResult: the PS 5.1 unrolling trap.
	  - TestListScopes_ShouldReturnEmptyForAServerWithNoScopes: "[\n\n]" is a valid
	    empty list, distinct from no output at all.
	  - TestListScopes_ShouldPreserveNonASCIIText: trap 6 on the Go side.
	  - TestListScopes_ShouldSortByWadaptID: the sort the pagination resume depends on.
	  - TestListScopes_ShouldDeriveAnIDForEveryScope: the omission half of the invariant.
	  - TestListScopes_ShouldSetTheAddressFamily: constant ipv4 in M3a.
	  - TestListScopes_ShouldTolerateStderrOnASuccessfulRun: stderr alone is not fatal.
	  - TestListScopes_ShouldRejectDuplicateDerivedIDs: the converse half of the
	    invariant — a collision is a repeated sort key, so it must be loud.
	  - TestListScopes_ShouldClassifyBackendFailures: exit, timeout, empty stdout
	    and undecodable output map to distinct typed errors.
	  - TestListScopes_ShouldAttachStderrAsContext: the operator gets the shell's
	    own words, bounded.
	  - TestListScopes_ShouldRunTheLiteralScript: no value is interpolated in.

	stderrContext
	  - TestStderrContext_ShouldTruncateOnARuneBoundary: bounded without corrupting
	    the very encoding this package exists to protect.

Tested elsewhere:

	Derivation stability and encoding: identity_test.go.
	The projection's shape and the script preamble: scope_test.go.
	Timeout and WaitDelay behaviour against a real process: runner_test.go.

Declined:

	Testing that ListScopes is safe for concurrent use: it holds only immutable
	  configuration and each call spawns its own process, so there is no shared
	  mutable state for a race detector to find. -race over the suite covers the
	  claim as well as a bespoke test would.

Additional Remarks:

	The fake runner lives here rather than in production code: a fake the binary
	  links is speculative code. internal/core/events/testing is the precedent if
	  another package ever genuinely needs to import one.
	Fixtures are read from testdata/ rather than inlined so they stay byte-for-byte
	  what the host emitted, including PS 5.1's two-space-after-colon formatting.
*/
package dhcpwindows

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testNamespaceKey is a fixed key so derived IDs are reproducible across runs.
const testNamespaceKey = "test-namespace-key-0123456789"

// fakeRunner replays canned output instead of spawning a shell, which is what
// keeps this package testable on darwin and task ci green there.
type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error

	// scripts records what was asked of it, so a test can assert on the script
	// text itself — notably that nothing was interpolated into it.
	scripts []string
}

func (f *fakeRunner) run(_ context.Context, script string) ([]byte, []byte, error) {
	f.scripts = append(f.scripts, script)

	return f.stdout, f.stderr, f.err
}

// fixture reads a captured PowerShell output file.
func fixture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	return raw
}

// clientWith returns a client backed by the given fake.
func clientWith(f *fakeRunner) *Client {
	return &Client{
		runner:       f,
		serverName:   "dhcp01.example.test",
		namespaceKey: []byte(testNamespaceKey),
	}
}

// clientReading returns a client replaying a fixture file.
func clientReading(t *testing.T, name string) *Client {
	t.Helper()

	return clientWith(&fakeRunner{stdout: fixture(t, name)})
}

func TestListScopes_ShouldDecodeTheCapturedFixture(t *testing.T) {
	t.Parallel()

	// ARRANGE — the byte-for-byte capture from the WS2022 host.
	client := clientReading(t, "scopes_single.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — every projected field lands on its model field. This is the
	// check that catches the projection and the struct tags drifting apart.
	require.NoError(t, err)
	require.Len(t, scopes, 1)

	got := scopes[0]
	assert.Equal(t, "192.168.178.0", got.ScopeID)
	assert.Equal(t, "255.255.255.0", got.SubnetMask)
	assert.Equal(t, "192.168.178.10", got.StartRange)
	assert.Equal(t, "192.168.178.210", got.EndRange)
	assert.Equal(t, "manual_test_01", got.Name)
	assert.Equal(t, "created by hand", got.Description)
	assert.Equal(t, "Active", got.State)
	assert.Equal(t, "Dhcp", got.Type)
	assert.Empty(t, got.SuperscopeName)
	assert.Equal(t, 691200, got.LeaseDurationSeconds)
}

func TestListScopes_ShouldDecodeASingleElementResult(t *testing.T) {
	t.Parallel()

	// ARRANGE — PS 5.1 unrolls a one-element pipeline into a bare object, which
	// would break the decoder. The script wraps in @() to prevent it; this
	// asserts the resulting shape still decodes as a list.
	client := clientReading(t, "scopes_single.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — a silent-corruption regression, not a loud one, so it stays
	// covered even though the workaround is verified on the host.
	require.NoError(t, err)
	assert.Len(t, scopes, 1)
}

func TestListScopes_ShouldReturnEmptyForAServerWithNoScopes(t *testing.T) {
	t.Parallel()

	// ARRANGE — the host emits "[\n\n]" for a server with no scopes.
	client := clientReading(t, "scopes_empty.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — valid JSON, decoded with no special handling. Distinct from no
	// output at all, which is an error.
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

func TestListScopes_ShouldPreserveNonASCIIText(t *testing.T) {
	t.Parallel()

	// ARRANGE
	client := clientReading(t, "scopes_nonascii.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — encoding/json substitutes U+FFFD for invalid UTF-8 rather than
	// erroring, so mojibake would decode "successfully". Asserting the exact
	// text is the only way to catch it.
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, "Standort München", scopes[0].Name)
	assert.Equal(t, "Büro Gebäude 3 – Süd", scopes[0].Description)
	assert.NotContains(t, scopes[0].Name, "�")
}

func TestListScopes_ShouldSortByWadaptID(t *testing.T) {
	t.Parallel()

	// ARRANGE
	client := clientReading(t, "scopes_multi.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — sorted by the encoded ID, which is what the pagination cursor
	// resumes on. Sorting by anything else (a numeric address order, say)
	// while resuming on the string would skip and repeat pages silently.
	require.NoError(t, err)
	require.Len(t, scopes, 3)

	ids := make([]string, 0, len(scopes))
	for _, s := range scopes {
		ids = append(ids, s.WadaptID)
	}

	assert.IsIncreasing(t, ids)
}

func TestListScopes_ShouldDeriveAnIDForEveryScope(t *testing.T) {
	t.Parallel()

	// ARRANGE
	client := clientReading(t, "scopes_multi.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — the omission half of the identity invariant. Under derivation it
	// holds by construction, so this guards the construction rather than a
	// state machine.
	require.NoError(t, err)
	require.Len(t, scopes, 3)

	for _, s := range scopes {
		assert.Len(t, s.WadaptID, WadaptIDLength, "scope %s has no usable identity", s.ScopeID)
	}
}

func TestListScopes_ShouldSetTheAddressFamily(t *testing.T) {
	t.Parallel()

	// ARRANGE — the v4 cmdlet does not report a family, so the client sets it.
	client := clientReading(t, "scopes_multi.json")

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — constant in M3a, present so IPv6 lands additively.
	require.NoError(t, err)

	for _, s := range scopes {
		assert.Equal(t, AddressFamilyIPv4, s.AddressFamily)
	}
}

func TestListScopes_ShouldTolerateStderrOnASuccessfulRun(t *testing.T) {
	t.Parallel()

	// ARRANGE — a benign line on stderr with a zero exit and good stdout.
	client := clientWith(&fakeRunner{
		stdout: fixture(t, "scopes_single.json"),
		stderr: []byte("WARNING: something the shell felt like mentioning\n"),
	})

	// ACT
	scopes, err := client.ListScopes(context.Background())

	// ASSERT — stderr's mere presence is deliberately not fatal. Making it so
	// before verifying it is silent on a real host would fail every request the
	// first time PS 5.1 wrote anything benign there.
	require.NoError(t, err)
	assert.Len(t, scopes, 1)
}

func TestListScopes_ShouldRejectDuplicateDerivedIDs(t *testing.T) {
	t.Parallel()

	// ARRANGE — the same scopeId twice derives the same ID. A real 64-bit HMAC
	// collision is negligible at DHCP scale and cannot be constructed here, so
	// the duplicate input stands in for it: identify() cannot tell the two
	// apart, which is the point.
	dup := `[{"scopeId":"10.0.5.0"},{"scopeId":"10.0.5.0"}]`
	client := clientWith(&fakeRunner{stdout: []byte(dup)})

	// ACT
	_, err := client.ListScopes(context.Background())

	// ASSERT — two scopes with one identity is also a repeated pagination sort
	// key, which would silently drop the rest of a walk at a page boundary.
	// Loud beats an unbounded silent failure.
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDuplicateWadaptID)
	assert.Contains(t, err.Error(), "10.0.5.0")
}

func TestListScopes_ShouldClassifyBackendFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fake    *fakeRunner
		wantErr error
	}{
		{
			name:    "the shell could not be started",
			fake:    &fakeRunner{err: exec.ErrNotFound},
			wantErr: ErrBackendUnavailable,
		},
		{
			name:    "the call exceeded its deadline",
			fake:    &fakeRunner{err: errors.Join(ErrBackendTimeout, context.DeadlineExceeded)},
			wantErr: ErrBackendTimeout,
		},
		{
			// The guard for a killed process, a crashed shell, or output
			// swallowed by a profile. A server with no scopes emits "[ ]", so
			// zero bytes can only be a failure.
			name:    "no output at all",
			fake:    &fakeRunner{stdout: nil},
			wantErr: ErrBackendMalformed,
		},
		{
			name:    "only whitespace",
			fake:    &fakeRunner{stdout: []byte("  \n\t ")},
			wantErr: ErrBackendMalformed,
		},
		{
			name:    "output that is not JSON",
			fake:    &fakeRunner{stdout: []byte("Get-DhcpServerv4Scope : Access is denied.")},
			wantErr: ErrBackendMalformed,
		},
		{
			// What a naive ConvertTo-Json of CimInstance output would produce:
			// valid JSON of the wrong shape.
			name:    "JSON of the wrong shape",
			fake:    &fakeRunner{stdout: []byte(`{"CimClass":"root/microsoft/windows/dhcp"}`)},
			wantErr: ErrBackendMalformed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := clientWith(tc.fake).ListScopes(context.Background())

			// ASSERT — distinct types so a handler can tell "unreachable" from
			// "spoke nonsense" rather than collapsing both into one 502.
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestListScopes_ShouldAttachStderrAsContext(t *testing.T) {
	t.Parallel()

	// ARRANGE — a failure with the shell's own explanation on stderr.
	client := clientWith(&fakeRunner{
		err:    exec.ErrNotFound,
		stderr: []byte("Get-DhcpServerv4Scope : Access is denied."),
	})

	// ACT
	_, err := client.ListScopes(context.Background())

	// ASSERT — the shell's words are the most useful thing an operator gets,
	// so they travel with the error rather than being dropped.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Access is denied")
}

func TestListScopes_ShouldRunTheLiteralScript(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fake := &fakeRunner{stdout: fixture(t, "scopes_single.json")}
	client := clientWith(fake)

	// ACT
	_, err := client.ListScopes(context.Background())

	// ASSERT — the script is a constant. M3a is backend-read-only and its
	// commands take no input at all, so the server name must reach the child
	// through the environment and never appear in the command text: that is
	// what keeps the injection surface at zero.
	require.NoError(t, err)
	require.Len(t, fake.scripts, 1)

	assert.Equal(t, listScopesScript, fake.scripts[0])
	assert.NotContains(t, fake.scripts[0], "dhcp01.example.test")
}

func TestStderrContext_ShouldTruncateOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// ARRANGE — well past the 512-rune bound, in multi-byte characters.
	noisy := strings.Repeat("ü", 900)

	// ACT
	got := stderrContext([]byte(noisy))

	// ASSERT — bounded, and still valid UTF-8. Truncating by bytes would split
	// a rune and put U+FFFD into the message describing an encoding problem.
	assert.True(t, strings.HasSuffix(got, "..."))
	assert.NotContains(t, got, "�")
	assert.Less(t, len(got), len(noisy))
}
