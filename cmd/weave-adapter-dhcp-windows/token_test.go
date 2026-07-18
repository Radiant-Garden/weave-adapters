/*
Testing: token.go

Pending:

Tested:
  runToken
    - TestRunToken_ShouldReportUsageWhenCommandMissingOrUnknown: no silent no-op.
    - TestRunToken_ShouldTreatHelpAsSuccess: help/-h/--help exit 0 at both levels.
  runTokenGen
    - TestRunTokenGen_ShouldMintStoreAndPrintTokenOnce: the token is shown but only its hash is stored.
    - TestRunTokenGen_ShouldSetExpiryWhenRequested: --expires-in-days lands on the entry.
    - TestRunTokenGen_ShouldRejectInvalidInput: missing label, negative days, duplicate label.
    - TestRunTokenGen_ShouldNotWriteFileWhenLabelDuplicate: a rejected add leaves the store untouched.
    - TestRunTokenGen_ShouldTellTheOperatorToRevokeWhenTheTokenCannotBeShown: a failed print names the way out.
  runTokenList
    - TestRunTokenList_ShouldReportEmptyStore: a fresh install is not an error.
    - TestRunTokenList_ShouldRenderExpiryStatus: never / expiring / expired columns.
    - TestRunTokenList_ShouldNeverPrintHashesOrTokens: list output carries no secret material.
    - TestRunTokenList_ShouldRoundEachDirectionTowardsTheTruth: elapsed rounds down, remaining rounds up.
  runTokenRevoke
    - TestRunTokenRevoke_ShouldRemoveNamedToken: removes one, keeps the rest.
    - TestRunTokenRevoke_ShouldReturnErrorWhenLabelUnknown: a typo fails loudly.
  loadOrEmpty
    - TestLoadOrEmpty_ShouldTreatMissingFileAsEmpty: covered via list/gen on a fresh path.
    - TestLoadOrEmpty_ShouldPropagateMalformedFile: a corrupt store is never overwritten.

Tested elsewhere:
  Token generation, hashing and persistence are covered in internal/core/auth;
  these tests cover only the CLI behavior layered on top.

Declined:
  printGenerated / describeExpiry / formatDays / formatElapsedDays / pluralDays /
  isHelpVerb / skipHelp — pure helpers, asserted through their commands' output
  rather than called directly.

Additional Remarks:
  Every test uses t.TempDir() and an explicit clock, so no test touches the
  developer's real tokens.toml and expiry rendering is deterministic.

  The "restart required" notice is asserted because restart-only rotation is a
  locked decision: an operator who is not told will read the CLI as broken.
*/

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
)

// cliClock is the fixed time CLI tests run at.
var cliClock = time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)

// fixedNow returns a clock function pinned to cliClock.
func fixedNow() func() time.Time {
	return func() time.Time { return cliClock }
}

// tokenFile returns a path inside a fresh temp dir.
func tokenFile(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "tokens.toml")
}

// runTokenCLI runs a token command and returns its output.
func runTokenCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	err := runToken(args, &out, fixedNow())

	return out.String(), err
}

func TestRunToken_ShouldReportUsageWhenCommandMissingOrUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "should report usage when no command is given", args: nil},
		{name: "should report usage when the command is unknown", args: []string{"delete"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			out, err := runTokenCLI(t, tt.args...)

			// ASSERT — the error is short; the usage goes to stdout.
			require.Error(t, err)
			assert.Contains(t, out, "gen")
			assert.Contains(t, out, "revoke")
		})
	}
}

func TestRunToken_ShouldTreatHelpAsSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "should print the command list for the help verb", args: []string{"help"}},
		{name: "should print the command list for -h", args: []string{"-h"}},
		{name: "should print the command list for --help", args: []string{"--help"}},
		{name: "should print subcommand flags for gen --help", args: []string{"gen", "--help"}},
		{name: "should print subcommand flags for list -h", args: []string{"list", "-h"}},
		{name: "should print subcommand flags for revoke --help", args: []string{"revoke", "--help"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			out, err := runTokenCLI(t, tt.args...)

			// ASSERT — asking for help is not a failure; exiting 1 here would be.
			require.NoError(t, err)
			assert.NotEmpty(t, out)
		})
	}
}

func TestRunTokenGen_ShouldMintStoreAndPrintTokenOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := tokenFile(t)

	// ACT
	out, err := runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path)
	require.NoError(t, err)

	// ASSERT — the token is printed with the scheme weave must send...
	token := extractToken(t, out)
	assert.Contains(t, out, "Bearer "+token)
	assert.Contains(t, out, restartNotice)

	// ...and only its hash reaches disk.
	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.NotContains(t, string(raw), token)

	store, err := auth.Load(path)
	require.NoError(t, err)
	require.Len(t, store.Tokens, 1)
	assert.Equal(t, "weave-prod", store.Tokens[0].Label)
	assert.Equal(t, auth.Hash(token), store.Tokens[0].Hash)
	assert.Nil(t, store.Tokens[0].ExpiresAt)
}

func TestRunTokenGen_ShouldSetExpiryWhenRequested(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := tokenFile(t)

	// ACT
	out, err := runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path, "--expires-in-days", "90")
	require.NoError(t, err)

	// ASSERT
	store, err := auth.Load(path)
	require.NoError(t, err)
	require.Len(t, store.Tokens, 1)
	require.NotNil(t, store.Tokens[0].ExpiresAt)

	want := cliClock.AddDate(0, 0, 90)
	assert.True(t, want.Equal(store.Tokens[0].ExpiresAt.Time()),
		"expected expiry %s, got %s", want, store.Tokens[0].ExpiresAt.Time())
	assert.Contains(t, out, "Expires")
}

func TestRunTokenGen_ShouldRejectInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "should reject when label is missing",
			args:    []string{"gen"},
			wantErr: "--label is required",
		},
		{
			name:    "should reject when expiry is negative",
			args:    []string{"gen", "--label", "weave-prod", "--expires-in-days", "-1"},
			wantErr: "must not be negative",
		},
		{
			name:    "should reject when the label is malformed",
			args:    []string{"gen", "--label", "weave prod"},
			wantErr: "label must be",
		},
		{
			// Left unchecked this mints an expiry past year 9999, which writes
			// a token file that every later Load rejects -- list, gen, revoke
			// and server startup alike -- until someone hand-edits it.
			name:    "should reject an expiry too far out to be read back",
			args:    []string{"gen", "--label", "weave-prod", "--expires-in-days", "9999999"},
			wantErr: "--expires-in-days 9999999 is too large",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			args := append(tt.args, "--file", tokenFile(t)) //nolint:gocritic // per-case copy is intended

			// ACT
			_, err := runTokenCLI(t, args...)

			// ASSERT
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRunTokenGen_ShouldNotWriteFileWhenLabelDuplicate(t *testing.T) {
	t.Parallel()

	// ARRANGE — one token already exists.
	path := tokenFile(t)
	_, err := runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path)
	require.NoError(t, err)

	before, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)

	// ACT
	_, err = runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path)

	// ASSERT — the live token must survive an accidental re-mint untouched.
	require.ErrorIs(t, err, auth.ErrDuplicateLabel)

	after, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestRunTokenList_ShouldReportEmptyStore(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a fresh install has no token file at all.
	out, err := runTokenCLI(t, "list", "--file", tokenFile(t))

	// ASSERT
	require.NoError(t, err)
	assert.Contains(t, out, "No tokens configured")
}

func TestRunTokenList_ShouldRenderExpiryStatus(t *testing.T) {
	t.Parallel()

	// ARRANGE — one never-expiring, one future, one already past.
	path := tokenFile(t)
	store := &auth.Store{Tokens: []auth.Entry{
		{Label: "forever", Hash: auth.Hash("a"), CreatedAt: cliClock},
		{Label: "soon", Hash: auth.Hash("b"), CreatedAt: cliClock, ExpiresAt: auth.NewExpiry(cliClock.AddDate(0, 0, 3))},
		{Label: "stale", Hash: auth.Hash("c"), CreatedAt: cliClock, ExpiresAt: auth.NewExpiry(cliClock.AddDate(0, 0, -2))},
	}}
	require.NoError(t, store.Save(path))

	// ACT
	out, err := runTokenCLI(t, "list", "--file", path)
	require.NoError(t, err)

	// ASSERT — an approaching expiry must be visible before requests fail.
	assert.Contains(t, out, "never")
	assert.Contains(t, out, "expires in 3 days")
	assert.Contains(t, out, "EXPIRED 2 days ago")
}

func TestRunTokenList_ShouldRoundEachDirectionTowardsTheTruth(t *testing.T) {
	t.Parallel()

	// ARRANGE — the fractional cases whole-day fixtures cannot distinguish:
	// exact multiples of 24h render the same however the rounding goes.
	path := tokenFile(t)
	store := &auth.Store{Tokens: []auth.Entry{
		{Label: "just-gone", Hash: auth.Hash("a"), CreatedAt: cliClock, ExpiresAt: auth.NewExpiry(cliClock.Add(-time.Hour))},
		{Label: "at-boundary", Hash: auth.Hash("b"), CreatedAt: cliClock, ExpiresAt: auth.NewExpiry(cliClock)},
		{Label: "nearly-gone", Hash: auth.Hash("c"), CreatedAt: cliClock, ExpiresAt: auth.NewExpiry(cliClock.Add(time.Hour))},
	}}
	require.NoError(t, store.Save(path))

	// ACT
	out, err := runTokenCLI(t, "list", "--file", path)
	require.NoError(t, err)

	// ASSERT — rounding an hour up to "1 day ago" points the operator at the
	// wrong day's logs, and rounding it down to "0 days ago" reads as a
	// rendering bug. Time remaining still rounds up, so a live token never
	// reports "0 days".
	assert.Contains(t, out, "EXPIRED less than a day ago")
	assert.NotContains(t, out, "EXPIRED 1 day ago")
	assert.NotContains(t, out, "EXPIRED 0 days ago")
	assert.Contains(t, out, "expires in 1 day")
}

func TestRunTokenGen_ShouldTellTheOperatorToRevokeWhenTheTokenCannotBeShown(t *testing.T) {
	t.Parallel()

	// ARRANGE — a writer that fails partway, standing in for a broken pipe or a
	// full disk after the store was already written.
	path := tokenFile(t)

	// ACT
	err := runToken([]string{"gen", "--label", "weave-prod", "--file", path}, failingWriter{}, fixedNow())

	// ASSERT — the label is occupied and its token is gone for good, so the
	// error has to name the way out rather than just reporting the write.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be recovered")
	assert.Contains(t, err.Error(), "token revoke --label weave-prod")

	// The entry really is on disk: this is guidance for a real state, not a
	// hypothetical one.
	store, loadErr := auth.Load(path)
	require.NoError(t, loadErr)

	_, found := store.Find("weave-prod")
	assert.True(t, found)
}

// failingWriter fails every write, like a closed pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestRunTokenList_ShouldNeverPrintHashesOrTokens(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := tokenFile(t)
	genOut, err := runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path)
	require.NoError(t, err)

	token := extractToken(t, genOut)

	// ACT
	out, err := runTokenCLI(t, "list", "--file", path)
	require.NoError(t, err)

	// ASSERT — listing is safe to paste into a ticket.
	assert.NotContains(t, out, token)
	assert.NotContains(t, out, auth.Hash(token))
	assert.Contains(t, out, "weave-prod")
}

func TestRunTokenRevoke_ShouldRemoveNamedToken(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := tokenFile(t)
	for _, label := range []string{"keep", "drop"} {
		_, err := runTokenCLI(t, "gen", "--label", label, "--file", path)
		require.NoError(t, err)
	}

	// ACT
	out, err := runTokenCLI(t, "revoke", "--label", "drop", "--file", path)
	require.NoError(t, err)

	// ASSERT
	assert.Contains(t, out, restartNotice)

	store, err := auth.Load(path)
	require.NoError(t, err)
	require.Len(t, store.Tokens, 1)
	assert.Equal(t, "keep", store.Tokens[0].Label)
}

func TestRunTokenRevoke_ShouldReturnErrorWhenLabelUnknown(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := tokenFile(t)
	_, err := runTokenCLI(t, "gen", "--label", "weave-prod", "--file", path)
	require.NoError(t, err)

	// ACT — a typo must not report success while the real token stays live.
	_, err = runTokenCLI(t, "revoke", "--label", "weave-prd", "--file", path)

	// ASSERT
	require.ErrorIs(t, err, auth.ErrUnknownLabel)

	store, err := auth.Load(path)
	require.NoError(t, err)
	assert.Len(t, store.Tokens, 1)
}

func TestLoadOrEmpty_ShouldTreatMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	store, err := loadOrEmpty(tokenFile(t))

	// ASSERT
	require.NoError(t, err)
	assert.Empty(t, store.Tokens)
}

func TestLoadOrEmpty_ShouldPropagateMalformedFile(t *testing.T) {
	t.Parallel()

	// ARRANGE — a corrupt store must never be treated as empty, or the next
	// gen would overwrite every configured token.
	path := tokenFile(t)
	require.NoError(t, os.WriteFile(path, []byte("not = = toml"), 0o600))

	// ACT
	_, err := loadOrEmpty(path)

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing token file")
}

// extractToken pulls the generated token out of gen's output.
func extractToken(t *testing.T, out string) string {
	t.Helper()

	for line := range strings.Lines(out) {
		if field := strings.TrimSpace(line); strings.HasPrefix(field, auth.TokenPrefix) {
			return field
		}
	}

	t.Fatalf("no token found in output:\n%s", out)

	return ""
}
