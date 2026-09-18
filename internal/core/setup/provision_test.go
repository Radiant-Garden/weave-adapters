/*
Testing: provision.go

Pending:

Tested:
  directoryStep
    - TestDirectoryStep_ShouldCreateAndLockTheDirectoryBeforeAnythingIsWrittenIntoIt: the ordering the whole step exists for.
    - TestDirectoryStep_ShouldLockADirectoryWhoseGrantsAreTooWide
    - TestDirectoryStep_ShouldLeaveAnOperatorsOwnLayoutAlone: a config elsewhere is locked per-file at install instead.
    - TestDirectoryStep_ShouldFailWhenTheTargetIsNotADirectory
  configStep
    - TestConfigStep_ShouldWriteAConfigThatResolvesToWhatWasProvisioned
    - TestConfigStep_ShouldValidateBeforeWritingRatherThanAfter: a refused configuration leaves no file, and no fresh key on disk.
    - TestConfigStep_ShouldNeverRewriteAnExistingConfig: byte-for-byte, comments included.
    - TestConfigStep_ShouldAcceptAProvisionedValueThatMatchesTheExistingOne: a re-run with the same flags finishes the job.
    - TestConfigStep_ShouldRefuseAProvisionedValueThatDiffersFromTheExistingOne: and never print either value.
    - TestConfigStep_ShouldFailOnAnExistingConfigThatWouldNotStart
    - TestConfigStep_ShouldRefuseToOverwriteAConfigThatAppearedAfterTheCheck: O_EXCL, not stat-then-write.
  tokenStep
    - TestTokenStep_ShouldMintAndSaveAndSecureTheStore
    - TestTokenStep_ShouldRequireARestartAfterMinting: buildAuth reads the store only at startup.
    - TestTokenStep_ShouldLeaveAStoreThatAlreadyHoldsAUsableToken
    - TestTokenStep_ShouldMintWhenEveryStoredTokenHasExpired: Usable, not len.
    - TestTokenStep_ShouldNameTheRevokeCommandWhenTheLabelExistsButHasExpired
    - TestTokenStep_ShouldSkipMintingWhenAuthIsDisabled: and say why.

Tested elsewhere:
  The mint sequence itself — Add-before-Save and the expiry round-trip:
  internal/core/auth/mint_test.go.

  The rendering these steps write: internal/core/config/render_test.go.

  What a lockdown does to a real Windows ACL: internal/core/winsvc, and task
  service-gate against a live host.

Declined:
  Driving the token step's Apply against a Windows-shaped store path. Save
  opens the file, so the path has to be one this host can write — which means
  these tests build their Values from a spec declaring authTokensFile as
  NotAPath. Asserting the service-path rule as well would be asserting
  CheckServicePaths, which owns it and tests it.

Additional Remarks:
  The config step's two branches are the ones to keep honest, and they fail in
  opposite directions. Writing when it should not rewrite destroys an
  operator's file and can re-key a fleet; refusing when it should write leaves
  a host unprovisioned with no way forward. The tests are written so that each
  branch's failure mode shows up as a failure of the other's test too.

  "Never rewritten" is asserted byte-for-byte rather than by comparing resolved
  values. go-toml would round-trip the VALUES perfectly while dropping every
  comment and every bit of formatting, which is exactly the damage the rule
  exists to prevent and exactly what a value comparison cannot see.
*/

package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// planFor builds a Plan over the given options and dependencies, as Run would.
func planFor(opts Options, deps Deps) *Plan { return newPlan(opts, deps) }

// localSpec is a spec whose path keys are NOT classified as paths, so a test
// can point them at a real temp directory.
//
// The service-path rule applies Windows rules on every host, which is correct
// and is tested where it lives. A step whose Apply actually opens the file
// needs a path this host can open, and reclassifying the key is the honest way
// to say "that rule is not what this test is about".
func localSpec() config.Spec {
	return config.Spec{
		{Name: config.KeyPort, Type: config.TypeInt, Default: 8444},
		{Name: config.KeyAuthTokensFile, Type: config.TypeString},
		{Name: config.KeyDisableAuth, Type: config.TypeBool},
	}
}

// localValues resolves a TOML body through localSpec.
func localValues(t *testing.T, body string) *config.Values {
	t.Helper()

	values, err := config.ResolveBytes(localSpec(), "config.toml", []byte(body))
	require.NoError(t, err)

	return values
}

// ---------------------------------------------------------------------------
// directory
// ---------------------------------------------------------------------------

func TestDirectoryStep_ShouldCreateAndLockTheDirectoryBeforeAnythingIsWrittenIntoIt(t *testing.T) {
	t.Parallel()

	// ARRANGE — a directory that does not exist yet.
	opts := runOptions(t)
	opts.Layout.Dir = filepath.Join(opts.Layout.Dir, "fresh")
	opts.Layout.ConfigPath = filepath.Join(opts.Layout.Dir, "config.toml")

	deps, _, sec := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := directoryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, directoryStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.DirExists(t, opts.Layout.Dir)

	// The container, with inheritable entries — not the files. On a default
	// C:\ProgramData ACL a file created by an elevated process inherits read
	// for local Users, so a config written first and secured afterwards is
	// world-readable in between, with identity.namespaceKey in it.
	require.Len(t, sec.Targets, 1)
	assert.Equal(t, opts.Layout.Dir, sec.Targets[0].Path)
	assert.Equal(t, winsvc.SecurableDirectory, sec.Targets[0].Kind)
	assert.Equal(t, winsvc.InheritToChildren, sec.Targets[0].Inheritance())
}

func TestDirectoryStep_ShouldLockADirectoryWhoseGrantsAreTooWide(t *testing.T) {
	t.Parallel()

	// ARRANGE — the directory exists, but somebody outside the policy can write.
	opts := runOptions(t)
	deps, _, _ := okDeps()
	deps.CheckDir = func(string) error { return winsvc.ErrNotSecured }

	// ACT
	verdict, err := directoryStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// Pending, not satisfied: an existing directory proves nothing about who
	// can write into it, and everything written afterwards inherits whatever
	// it permits.
	require.NoError(t, err)
	assert.Equal(t, Pending, verdict.Condition)
	assert.Contains(t, verdict.Detail, "re-lock")
}

func TestDirectoryStep_ShouldLeaveAnOperatorsOwnLayoutAlone(t *testing.T) {
	t.Parallel()

	// ARRANGE — a config path of the operator's, not the layout's, and a
	// layout directory that does not exist, so "left alone" is visible.
	opts := runOptions(t)
	opts.ConfigPath = filepath.Join(t.TempDir(), "their-config.toml")
	opts.Layout.Dir = filepath.Join(opts.Layout.Dir, "never-created")
	opts.Layout.ConfigPath = filepath.Join(opts.Layout.Dir, "config.toml")

	deps, _, sec := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := directoryStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, directoryStep{}.Apply(context.Background(), p))

	// ASSERT
	// Creating and locking a directory the operator never asked for would be
	// noise; their files are covered by the per-file lockdown at install,
	// derived from their own resolved values.
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.Empty(t, sec.Targets)
	assert.NoDirExists(t, opts.Layout.Dir)
}

func TestDirectoryStep_ShouldFailWhenTheTargetIsNotADirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE — a FILE where the provisioning directory should be.
	opts := runOptions(t)
	opts.Layout.Dir = filepath.Join(t.TempDir(), "in-the-way")
	opts.Layout.ConfigPath = filepath.Join(opts.Layout.Dir, "config.toml")
	require.NoError(t, os.WriteFile(opts.Layout.Dir, []byte("x"), 0o600))

	deps, _, _ := okDeps()

	// ACT
	_, err := directoryStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// An error rather than a condition: no flag makes this proceed, and
	// MkdirAll would fail later with a message about the parent instead.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func TestConfigStep_ShouldWriteAConfigThatResolvesToWhatWasProvisioned(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := configStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, configStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.FileExists(t, opts.Layout.ConfigPath)

	// Re-resolved from the bytes on disk rather than from the projection: what
	// the service will read is the file, and anything that made the two differ
	// is worth finding now.
	require.NotNil(t, p.Values)
	assert.Equal(t, testTokenStore, p.Values.String(config.KeyAuthTokensFile))
	assert.Equal(t, testLogFile, p.Values.String(config.KeyLogFile))
	assert.Equal(t, "set", p.Values.String(testAdapterKey))
}

func TestConfigStep_ShouldValidateBeforeWritingRatherThanAfter(t *testing.T) {
	t.Parallel()

	// ARRANGE — a configuration the adapter refuses.
	opts := runOptions(t)
	opts.Provisioned = []config.Provisioned{
		{Key: config.KeyAuthTokensFile, Value: testTokenStore},
		{Key: config.KeyLogFile, Value: testLogFile},
		// testAdapterKey deliberately absent, which validateTestAdapter rejects.
	}

	deps, _, _ := okDeps()

	// ACT
	_, err := configStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// Render to memory, validate, and only then write. The other order leaves
	// a failed run with a config on disk carrying a freshly generated
	// identity.namespaceKey nobody asked for — and on the next run that key is
	// indistinguishable from a provisioned one, so the mistake is not
	// recoverable by re-running.
	require.Error(t, err)
	assert.Contains(t, err.Error(), testAdapterKey)
	assert.NoFileExists(t, opts.Layout.ConfigPath, "a rejected configuration was written anyway")
}

func TestConfigStep_ShouldNeverRewriteAnExistingConfig(t *testing.T) {
	t.Parallel()

	// ARRANGE — an operator's file, with their comments and their spacing.
	opts := runOptions(t)

	original := "# the comment an operator wrote\n" +
		"authTokensFile   =   '" + testTokenStore + "'\n\n" +
		"logFile = '" + testLogFile + "'\n" +
		"[fake]\nrequired = 'set'   # and this one\n"
	require.NoError(t, os.WriteFile(opts.Layout.ConfigPath, []byte(original), 0o600))

	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	// ACT
	verdict, err := configStep{}.Check(context.Background(), p)

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.True(t, p.ConfigExists)

	// Byte-for-byte, not value-for-value. go-toml round-trips every VALUE
	// perfectly while dropping every comment and all formatting, so a
	// comparison of resolved values cannot see the damage this rule prevents.
	after, err := os.ReadFile(opts.Layout.ConfigPath)
	require.NoError(t, err)
	assert.Equal(t, original, string(after))
}

func TestConfigStep_ShouldAcceptAProvisionedValueThatMatchesTheExistingOne(t *testing.T) {
	t.Parallel()

	// ARRANGE — a re-run passing exactly the flags the first run was given.
	opts := runOptions(t)
	body := "authTokensFile = '" + testTokenStore + "'\nlogFile = '" + testLogFile + "'\n" +
		"[fake]\nrequired = 'set'\n"
	require.NoError(t, os.WriteFile(opts.Layout.ConfigPath, []byte(body), 0o600))

	deps, _, _ := okDeps()

	// ACT
	verdict, err := configStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// Re-running with the same command line has to finish the job rather than
	// fail on its own flags — that is the whole promise of re-running on a
	// half-configured host. An identical value is a no-op, not a conflict.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
}

func TestConfigStep_ShouldRefuseAProvisionedValueThatDiffersFromTheExistingOne(t *testing.T) {
	t.Parallel()

	// ARRANGE — a different namespace key than the file already carries.
	opts := runOptions(t)
	opts.Provisioned = append(opts.Provisioned, config.Provisioned{
		Key: testAdapterKey, Value: "a-different-value",
	})
	opts.Provisioned = opts.Provisioned[1:] // drop the duplicate testAdapterKey

	body := "authTokensFile = '" + testTokenStore + "'\nlogFile = '" + testLogFile + "'\n" +
		"[fake]\nrequired = 'the-one-already-there'\n"
	require.NoError(t, os.WriteFile(opts.Layout.ConfigPath, []byte(body), 0o600))

	deps, _, _ := okDeps()

	// ACT
	_, err := configStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	require.Error(t, err)
	assert.Contains(t, err.Error(), testAdapterKey)

	// Neither half of the comparison is ever shown. One of these keys is
	// identity.namespaceKey on a real host, and an error that printed it would
	// put a backup-critical secret into scrollback and into an MSI log.
	assert.NotContains(t, err.Error(), "a-different-value")
	assert.NotContains(t, err.Error(), "the-one-already-there")
}

func TestConfigStep_ShouldFailOnAnExistingConfigThatWouldNotStart(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.Provisioned = nil

	body := "authTokensFile = 'tokens.toml'\nlogFile = '" + testLogFile + "'\n[fake]\nrequired = 'set'\n"
	require.NoError(t, os.WriteFile(opts.Layout.ConfigPath, []byte(body), 0o600))

	deps, _, _ := okDeps()

	// ACT
	_, err := configStep{}.Check(context.Background(), planFor(opts, deps))

	// ASSERT
	// An error, not Blocked. Blocked means "will not, without a flag", and no
	// flag can make an unstartable configuration start — the operator has to
	// edit the file.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would not start as a service")
	assert.Contains(t, err.Error(), config.KeyAuthTokensFile)
}

func TestConfigStep_ShouldRefuseToOverwriteAConfigThatAppearedAfterTheCheck(t *testing.T) {
	t.Parallel()

	// ARRANGE — checked on an empty host, then somebody else writes the file.
	opts := runOptions(t)
	deps, _, _ := okDeps()
	p := planFor(opts, deps)

	verdict, err := configStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, Pending, verdict.Condition)

	theirs := "# written between the check and the apply\n"
	require.NoError(t, os.WriteFile(opts.Layout.ConfigPath, []byte(theirs), 0o600))

	// ACT
	err = configStep{}.Apply(context.Background(), p)

	// ASSERT
	// O_EXCL rather than a second stat: exclusive creation is the only form in
	// which "never rewrite an existing config" is not a race, and the file it
	// would have destroyed may be the only copy of a namespace key.
	require.Error(t, err)

	after, readErr := os.ReadFile(opts.Layout.ConfigPath)
	require.NoError(t, readErr)
	assert.Equal(t, theirs, string(after))
}

// ---------------------------------------------------------------------------
// token
// ---------------------------------------------------------------------------

func TestTokenStep_ShouldMintAndSaveAndSecureTheStore(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := filepath.Join(t.TempDir(), "tokens.toml")

	opts := runOptions(t)
	opts.Spec = localSpec()

	deps, _, sec := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "authTokensFile = '"+store+"'\n")

	// ACT
	verdict, err := tokenStep{}.Check(context.Background(), p)
	require.NoError(t, err)
	require.NoError(t, tokenStep{}.Apply(context.Background(), p))

	// ASSERT
	assert.Equal(t, Pending, verdict.Condition)
	assert.NotEmpty(t, p.Token, "the token never reached the plan, and nothing can recover it")

	saved, err := auth.Load(store)
	require.NoError(t, err)
	require.Len(t, saved.Tokens, 1)
	assert.Equal(t, opts.TokenLabel, saved.Tokens[0].Label)

	// The hash, never the token — the whole reason the store is not a
	// credential.
	assert.Equal(t, auth.Hash(p.Token), saved.Tokens[0].Hash)

	// The FILE, not only its directory. The store did not exist when the
	// directory was locked, and the gate asserts the file is protected rather
	// than merely inheriting.
	require.Len(t, sec.Targets, 1)
	assert.Equal(t, store, sec.Targets[0].Path)
	assert.Equal(t, winsvc.SecurableFile, sec.Targets[0].Kind)
}

func TestTokenStep_ShouldRequireARestartAfterMinting(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.Spec = localSpec()

	deps, _, _ := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "authTokensFile = '"+filepath.Join(t.TempDir(), "tokens.toml")+"'\n")

	// ACT
	require.NoError(t, tokenStep{}.Apply(context.Background(), p))

	// ASSERT
	// buildAuth reads the store once, at startup. Rotation is restart-only by
	// design, so a token minted against a running service does nothing until
	// it restarts — and an operator not told that concludes the token is
	// broken.
	assert.NotEmpty(t, p.restartWanted)
	assert.Contains(t, p.restartWanted, "startup")
}

func TestTokenStep_ShouldLeaveAStoreThatAlreadyHoldsAUsableToken(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := filepath.Join(t.TempDir(), "tokens.toml")

	existing := &auth.Store{}
	_, err := existing.Mint("someone-elses-label", time.Now(), nil)
	require.NoError(t, err)
	require.NoError(t, existing.Save(store))

	opts := runOptions(t)
	opts.Spec = localSpec()

	deps, _, _ := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "authTokensFile = '"+store+"'\n")

	// ACT
	verdict, err := tokenStep{}.Check(context.Background(), p)

	// ASSERT
	// A usable token is a usable token whatever it is labelled. Minting a
	// second one because the label differs would hand the operator a
	// credential nobody asked for, and a restart they did not need.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.Empty(t, p.Token)
}

func TestTokenStep_ShouldMintWhenEveryStoredTokenHasExpired(t *testing.T) {
	t.Parallel()

	// ARRANGE — a store that plainly contains a token, which is expired.
	store := filepath.Join(t.TempDir(), "tokens.toml")
	past := time.Now().Add(-48 * time.Hour)

	existing := &auth.Store{}
	_, err := existing.Mint("stale", past.Add(-24*time.Hour), auth.NewExpiry(past))
	require.NoError(t, err)
	require.NoError(t, existing.Save(store))

	opts := runOptions(t)
	opts.Spec = localSpec()

	deps, _, _ := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "authTokensFile = '"+store+"'\n")

	// ACT
	verdict, err := tokenStep{}.Check(context.Background(), p)

	// ASSERT
	// Usable, not len. A store whose tokens have all expired refuses every
	// request and stops the adapter starting, while visibly containing tokens
	// — counting entries would report "already done" for the harder of the two
	// failures to diagnose.
	require.NoError(t, err)
	assert.Equal(t, Pending, verdict.Condition)
}

func TestTokenStep_ShouldNameTheRevokeCommandWhenTheLabelExistsButHasExpired(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := filepath.Join(t.TempDir(), "tokens.toml")
	past := time.Now().Add(-48 * time.Hour)

	opts := runOptions(t)
	opts.Spec = localSpec()

	existing := &auth.Store{}
	_, err := existing.Mint(opts.TokenLabel, past.Add(-24*time.Hour), auth.NewExpiry(past))
	require.NoError(t, err)
	require.NoError(t, existing.Save(store))

	deps, _, _ := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "authTokensFile = '"+store+"'\n")

	// ACT
	_, err = tokenStep{}.Check(context.Background(), p)

	// ASSERT
	// Store.Add refuses a duplicate label, so a re-run here would otherwise
	// fail at the mint with a duplicate error that reads like a bug. Saying so
	// at the check, with the way out, is the difference between that and a
	// two-minute fix.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
	assert.Contains(t, err.Error(), "token revoke --label "+opts.TokenLabel)
}

func TestTokenStep_ShouldSkipMintingWhenAuthIsDisabled(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := filepath.Join(t.TempDir(), "tokens.toml")

	opts := runOptions(t)
	opts.Spec = localSpec()

	deps, _, _ := okDeps()
	p := planFor(opts, deps)
	p.Values = localValues(t, "disableAuth = true\nauthTokensFile = '"+store+"'\n")

	// ACT
	verdict, err := tokenStep{}.Check(context.Background(), p)

	// ASSERT
	// Minting into a store nothing reads and then reporting success is worse
	// than doing nothing: it leaves an operator holding a token that will
	// never be checked, and a verification step that cannot mean anything.
	require.NoError(t, err)
	assert.Equal(t, Satisfied, verdict.Condition)
	assert.Contains(t, verdict.Detail, config.KeyDisableAuth)
	assert.NoFileExists(t, store)
}
