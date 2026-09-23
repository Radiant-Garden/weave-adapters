/*
Testing: setupreport.go

Pending:

Tested:
  printSetupReport
    - TestPrintSetupReport_ShouldNameEveryStepAndItsCondition
    - TestPrintSetupReport_ShouldDistinguishAnAppliedStepFromAPendingOne: after the applying pass they mean opposite things.
    - TestPrintSetupReport_ShouldShowOnlyTheFirstLineOfAFailure: the plan must stay readable.
  printBlocked
    - TestPrintBlocked_ShouldNameTheFlagThatClearsEachBlock
  reportToken
    - TestReportToken_ShouldShowAMintedTokenOnceWithItsScheme
    - TestReportToken_ShouldSayNothingWhenNothingWasMinted
    - TestReportToken_ShouldWriteTheTokenWhenAskedTo
    - TestReportToken_ShouldRefuseToOverwriteAnExistingTokenFile: O_EXCL, and the token is still shown.
    - TestReportToken_ShouldLockTheFileDownBeforeWritingTheCredentialIntoIt
    - TestReportToken_ShouldNotLeaveAFileBehindWhenTheLockdownFails
  printGeneratedKey
    - TestPrintGeneratedKey_ShouldShowTheFingerprintAndNeverTheKey
    - TestPrintGeneratedKey_ShouldSayNothingForAKeyThatWasSupplied
  printProvisioned
    - TestPrintProvisioned_ShouldNameASecretWithoutShowingIt
  printHealth
    - TestPrintHealth_ShouldExplainAnUnhealthyBackendRatherThanJustReportingIt

Tested elsewhere:
  Everything the report describes: internal/core/setup produces the Result, and
  its own tests assert what each field means.

  The exit codes these messages accompany: setup_test.go.

Declined:
  Pinning the exact column widths or the wording of a sentence. Both are
  presentation; a test that froze them would fail on a rewording and would
  prove nothing about whether an operator can act on what they read.

Additional Remarks:
  Two assertions here are security assertions wearing the clothes of output
  tests, and neither may be relaxed.

  identity.namespaceKey must never reach the output. PowerShell transcription
  is routine Group Policy in exactly these environments, scrollback persists,
  and an MSI log lands in a world-readable %TEMP% — so a key that is printed is
  a key with a second copy somewhere less protected than the ACL'd file it had
  to be written to anyway. The fingerprint carries the whole affordance without
  the risk.

  A written token file is created with O_EXCL, locked down, and only THEN
  written into. It is a live credential at rest, and the one thing worse than
  refusing to write over an existing file is destroying whatever was there.

  The lock-then-write order is a security assertion too. A file created under
  a destination directory's inherited grants is readable by whoever that
  directory admits, so writing first left the credential in plaintext under
  those grants until the lockdown landed — and a lockdown that then failed
  returned an error and left the plaintext behind. Creating it empty means the
  only thing ever exposed is a zero-byte file.
*/

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/setup"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc/winsvctest"
)

// noSecure is a lockdown that records nothing and refuses nothing, for the
// report tests that are not about what gets secured.
func noSecure() setup.SecureFunc { return (&winsvctest.Securer{}).Secure }

// testToken is a stand-in bearer token. It carries the real prefix so the
// assertions read like the real output, and is not a credential: nothing
// accepts it.
//
//nolint:gosec // G101: a literal in a test, not a credential.
const testToken = "wadapt_a-token-this-run-minted"

// captured runs f against a fresh printer and returns what it wrote.
func captured(f func(*printer)) string {
	var out bytes.Buffer

	f(&printer{w: &out})

	return out.String()
}

func TestPrintSetupReport_ShouldNameEveryStepAndItsCondition(t *testing.T) {
	t.Parallel()

	// ARRANGE
	result := setup.Result{
		DryRun: true,
		Steps: []setup.StepResult{
			{Name: "config", Verdict: setup.Verdict{Condition: setup.Pending, Detail: "write it"}},
			{Name: "token", Verdict: setup.Verdict{Condition: setup.Satisfied, Detail: "already there"}},
		},
	}

	// ACT
	out := captured(func(p *printer) { printSetupReport(p, result) })

	// ASSERT
	// The detail is what makes the report usable: seven step names and three
	// conditions say nothing about WHICH registration differs.
	assert.Contains(t, out, "config")
	assert.Contains(t, out, "pending")
	assert.Contains(t, out, "write it")
	assert.Contains(t, out, "satisfied")
	assert.Contains(t, out, "Dry run")
}

func TestPrintSetupReport_ShouldDistinguishAnAppliedStepFromAPendingOne(t *testing.T) {
	t.Parallel()

	// ARRANGE — a real run that stopped partway.
	result := setup.Result{
		Steps: []setup.StepResult{
			{Name: "config", Verdict: setup.Verdict{Condition: setup.Pending, Detail: "wrote it"}, Applied: true},
			{Name: "token", Verdict: setup.Verdict{Condition: setup.Pending, Detail: "would mint"}},
		},
	}

	// ACT
	out := captured(func(p *printer) { printSetupReport(p, result) })

	// ASSERT
	// After the applying pass these mean opposite things: "done" happened,
	// while "pending" means the run stopped before reaching it.
	assert.Contains(t, out, "done")
	assert.Contains(t, out, "pending")
}

func TestPrintSetupReport_ShouldShowOnlyTheFirstLineOfAFailure(t *testing.T) {
	t.Parallel()

	// ARRANGE — a configuration failure joins one message per bad key.
	result := setup.Result{
		Steps: []setup.StepResult{
			{Name: "config", Err: errors.New("would not start:\nauthTokensFile is relative\nlogFile is relative")},
		},
	}

	// ACT
	out := captured(func(p *printer) { printSetupReport(p, result) })

	// ASSERT
	// main prints the whole error to stderr. Repeating it in full here turns a
	// seven-line plan into a page and buries the plan it exists to show.
	assert.Contains(t, out, "would not start:")
	assert.Contains(t, out, "[…]")
	assert.NotContains(t, out, "logFile is relative")
}

func TestPrintBlocked_ShouldNameTheFlagThatClearsEachBlock(t *testing.T) {
	t.Parallel()

	// ARRANGE
	result := setup.Result{
		Blocked: []setup.StepResult{
			{Name: "install", Verdict: setup.Verdict{Condition: setup.Blocked, Detail: "runs something else; --reinstall"}},
		},
	}

	// ACT
	out := captured(func(p *printer) { printBlocked(p, result) })

	// ASSERT
	// A Blocked verdict without the flag that clears it is a dead end wearing
	// a status.
	assert.Contains(t, out, "install")
	assert.Contains(t, out, "--reinstall")
	assert.Contains(t, out, "Nothing was changed")
}

func TestReportToken_ShouldShowAMintedTokenOnceWithItsScheme(t *testing.T) {
	t.Parallel()

	// ARRANGE
	token := testToken
	result := setup.Result{Token: token}
	opts := setup.Options{TokenLabel: "weave-prod"}

	// ACT
	var out bytes.Buffer

	p := &printer{w: &out}
	require.NoError(t, reportToken(p, result, opts, "", noSecure()))

	// ASSERT
	// weave sends this value verbatim — its credential store does not prepend
	// a scheme — so the full header value is shown rather than the bare token.
	assert.Contains(t, out.String(), token)
	assert.Contains(t, out.String(), "Bearer "+token)
	assert.Contains(t, out.String(), "only time")
}

func TestReportToken_ShouldSayNothingWhenNothingWasMinted(t *testing.T) {
	t.Parallel()

	// ARRANGE — a run that found a usable token already in the store, and so
	// never saw one: the store keeps only hashes.
	var out bytes.Buffer

	p := &printer{w: &out}

	// ACT
	require.NoError(t, reportToken(p, setup.Result{}, setup.Options{TokenLabel: "weave-prod"}, "", noSecure()))

	// ASSERT
	assert.Empty(t, out.String())
}

func TestReportToken_ShouldWriteTheTokenWhenAskedTo(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "token")
	token := testToken

	var out bytes.Buffer

	p := &printer{w: &out}
	sec := &winsvctest.Securer{}

	// ACT
	require.NoError(t, reportToken(p, setup.Result{Token: token}, setup.Options{TokenLabel: "weave-prod"}, path, sec.Secure))

	// ASSERT
	// Opt-in, for an operator handing it to a secret manager or removable
	// medium — and the warning is not decoration: nobody deletes that file
	// afterwards unless they are told to.
	written, err := os.ReadFile(path) //nolint:gosec // a path this test created
	require.NoError(t, err)
	assert.Contains(t, string(written), token)
	assert.Contains(t, out.String(), "LIVE CREDENTIAL")
}

func TestReportToken_ShouldRefuseToOverwriteAnExistingTokenFile(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("something already here"), 0o600))

	var out bytes.Buffer

	p := &printer{w: &out}
	sec := &winsvctest.Securer{}
	token := testToken

	// ACT
	err := reportToken(p, setup.Result{Token: token}, setup.Options{TokenLabel: "weave-prod"}, path, sec.Secure)

	// ASSERT
	// O_EXCL: an operator who points this at an existing file has almost
	// certainly made a mistake, and the one thing worse than refusing is
	// destroying whatever was there.
	require.Error(t, err)

	survived, readErr := os.ReadFile(path) //nolint:gosec // a path this test created
	require.NoError(t, readErr)
	assert.Equal(t, "something already here", string(survived))

	// The token was minted and is already in the store, so it must still have
	// been shown — otherwise it is lost for a file-writing failure.
	assert.Contains(t, out.String(), token)
}

func TestPrintGeneratedKey_ShouldShowTheFingerprintAndNeverTheKey(t *testing.T) {
	t.Parallel()

	// ARRANGE
	key := "a-generated-namespace-key-0123456789"
	provisioned := []config.Provisioned{
		{Key: dhcpwindows.KeyNamespaceKey, Value: key, Secret: true, FreshlyGenerated: true},
	}

	// ACT
	out := captured(func(p *printer) { printGeneratedKey(p, provisioned, `C:\ProgramData\x\config.toml`) })

	// ASSERT
	assertNoKeyInOutput(t, out, key)

	// The same fingerprint DHCP-001 logs at startup, derived by the adapter
	// rather than re-derived here, so an operator can match the two.
	assert.Contains(t, out, dhcpwindows.NamespaceKeyFingerprint(key))
	assert.Contains(t, out, "BACK THAT FILE UP")
	assert.Contains(t, out, `C:\ProgramData\x\config.toml`)
}

func TestPrintGeneratedKey_ShouldSayNothingForAKeyThatWasSupplied(t *testing.T) {
	t.Parallel()

	// ARRANGE — supplied from a file, not invented here.
	provisioned := []config.Provisioned{
		{Key: dhcpwindows.KeyNamespaceKey, Value: "a-supplied-key", Secret: true},
	}

	// ACT
	out := captured(func(p *printer) { printGeneratedKey(p, provisioned, "config.toml") })

	// ASSERT
	// The back-it-up advice is for a key that exists nowhere else yet. An
	// operator who supplied one from a file already has it.
	assert.Empty(t, out)
}

func TestPrintProvisioned_ShouldNameASecretWithoutShowingIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	key := "a-generated-namespace-key-0123456789"
	provisioned := []config.Provisioned{
		{Key: config.KeyLogFile, Value: `C:\logs\adapter.log`},
		{Key: dhcpwindows.KeyNamespaceKey, Value: key, Secret: true},
	}

	// ACT
	out := captured(func(p *printer) { printProvisioned(p, provisioned) })

	// ASSERT
	assertNoKeyInOutput(t, out, key)
	assert.Contains(t, out, dhcpwindows.KeyNamespaceKey, "an operator has to know it was set")
	assert.Contains(t, out, "not shown")
	assert.Contains(t, out, `C:\logs\adapter.log`, "a path is not a secret and hiding it helps nobody")
}

func TestPrintHealth_ShouldExplainAnUnhealthyBackendRatherThanJustReportingIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	result := setup.Result{
		Health: setup.HealthReport{
			Checked: true, Reachable: true, Component: healthComponent, Detail: "unhealthy: no backend",
		},
	}

	// ACT
	out := captured(func(p *printer) { printHealth(p, result) })

	// ASSERT
	// It is the normal answer on every host without a DHCP server, including
	// every CI runner. An operator who read only a non-zero exit would undo a
	// correct installation.
	assert.Contains(t, out, "installed and running")
	assert.Contains(t, out, "no reachable DHCP backend")
	assert.Contains(t, out, "The service itself is fine")
}

func TestReportToken_ShouldLockTheFileDownBeforeWritingTheCredentialIntoIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The securer reads the file as it is locked down, which is the moment the
	// old order had a plaintext credential sitting under the destination
	// directory's inherited grants.
	path := filepath.Join(t.TempDir(), "token")

	var atLockdown []byte

	secure := func(targets []winsvc.Securable) ([]winsvc.SecureResult, error) {
		atLockdown, _ = os.ReadFile(targets[0].Path)

		return []winsvc.SecureResult{{Target: targets[0], Applied: true}}, nil
	}

	var out bytes.Buffer

	// ACT
	require.NoError(t, reportToken(&printer{w: &out},
		setup.Result{Token: testToken}, setup.Options{TokenLabel: "weave-prod"}, path, secure))

	// ASSERT
	// Empty at the lockdown, and the credential there afterwards. A file
	// carrying the token before it is locked is readable by whoever the
	// directory admits for as long as the lockdown takes.
	assert.Empty(t, atLockdown, "the credential was on disk before the lockdown ran")

	written, err := os.ReadFile(path) //nolint:gosec // a path this test created
	require.NoError(t, err)
	assert.Contains(t, string(written), testToken)
}

func TestReportToken_ShouldNotLeaveAFileBehindWhenTheLockdownFails(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := filepath.Join(t.TempDir(), "token")
	sec := &winsvctest.Securer{Err: errors.New("this needs an elevated prompt")}

	var out bytes.Buffer

	// ACT
	err := reportToken(&printer{w: &out},
		setup.Result{Token: testToken}, setup.Options{TokenLabel: "weave-prod"}, path, sec.Secure)

	// ASSERT
	// The file is this call's to remove — O_EXCL proves nothing else created
	// it — and an orphan left under the directory's inherited grants is both a
	// credential nobody knows is there and the reason the next run is told the
	// destination is occupied.
	require.Error(t, err)
	assert.NoFileExists(t, path)

	// The token was minted and is already a hash in the store, so it must
	// still be shown: losing it here means an operator has a credential they
	// cannot use and a label they cannot reuse.
	assert.Contains(t, out.String(), testToken)
}
