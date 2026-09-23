/*
Testing: secure.go

Pending:

Tested:
  SecurePaths
    - TestSecurePaths_ShouldCoverEveryPathTheConfigurationNames: all three, derived from the resolved values.
    - TestSecurePaths_ShouldSecureTheLogDirectoryRatherThanTheLogFile: the adapter creates the file, so the directory's entries decide its ACL.
    - TestSecurePaths_ShouldOmitPathsTheConfigurationDoesNotSet: an unset target is not a "" path handed to the lockdown.
    - TestSecurePaths_ShouldPropagateAFailure: a failed lockdown is never reported as coverage.
    - TestSecurePaths_ShouldRefuseALockdownAimedAtAVolumeRoot: before anything is applied, because this one is not undone by noticing.

Tested elsewhere:
  What the lockdown actually does to a Windows ACL: internal/core/winsvc's
  secure_windows tests, and task service-gate against a live host.

  Which targets are correct, and why each one matters: winsvc.SecurablesFor
  owns that list and is tested with it.

  The two commands layered on this — install securing after registration, and
  the standalone lockdown verb — in install_test.go and the service
  subcommand's tests.

Declined:
  Asserting the Why strings. They are operator-facing wording owned by
  SecurablesFor; pinning them here would fail on a rewording that changed
  nothing.

Additional Remarks:
  The refusal runs BEFORE deps.Secure rather than inside it, and the ordering
  is the assertion: the entries the lockdown applies are protected and
  inheriting, so a directory target that turned out to be a volume root would
  already have had the whole subtree's inherited grants replaced by the time
  anything reported it.

  The point of this function is that the targets come from the RESOLVED
  configuration rather than from what a caller was told. That is the argument
  for the installer being a Go subcommand rather than a script: a script has to
  be handed all three paths and drifts from them silently. The first test is
  the one that would catch that drift.
*/

package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// resolve loads a config body through the synthetic spec install_test.go
// composes, so these tests exercise the same resolution Install does.
func resolve(t *testing.T, body string) (string, *config.Values) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	values, err := config.LoadWithoutEnvironment(testSpec(), []string{"--config", path})
	require.NoError(t, err)

	return path, values
}

func TestSecurePaths_ShouldCoverEveryPathTheConfigurationNames(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, sec := okDeps()
	cfg, values := resolve(t,
		"authTokensFile = '"+testTokenStore+"'\nlogFile = '"+testLogFile+"'\n")

	// ACT
	results, err := SecurePaths(deps, cfg, values)

	// ASSERT
	// All three, derived from the configuration that was just resolved —
	// which is the whole argument for doing this in the binary rather than a
	// script that has to be told the paths.
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Len(t, sec.Targets, 3)

	paths := make([]string, 0, len(sec.Targets))
	for _, target := range sec.Targets {
		paths = append(paths, target.Path)
	}

	assert.Contains(t, paths, cfg)
	assert.Contains(t, paths, testTokenStore)
}

func TestSecurePaths_ShouldSecureTheLogDirectoryRatherThanTheLogFile(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, sec := okDeps()
	cfg, values := resolve(t,
		"authTokensFile = '"+testTokenStore+"'\nlogFile = '"+testLogFile+"'\n")

	// ACT
	_, err := SecurePaths(deps, cfg, values)

	// ASSERT
	// The adapter creates the log itself, at runtime, and a new file inherits
	// its parent's inheritable entries. A file-only grant would leave the log
	// under whatever the directory's default permits — the ACL the lockdown
	// was replacing.
	require.NoError(t, err)

	paths := make([]string, 0, len(sec.Targets))
	for _, target := range sec.Targets {
		paths = append(paths, target.Path)
	}

	// testRoot rather than filepath.Dir: the derivation is Windows', not the
	// host's, so filepath.Dir off Windows sees no separator in this path at
	// all and answers "." — which is what this assertion used to accept.
	assert.Contains(t, paths, testRoot)
	assert.NotContains(t, paths, testLogFile)
}

func TestSecurePaths_ShouldOmitPathsTheConfigurationDoesNotSet(t *testing.T) {
	t.Parallel()

	// ARRANGE — no logFile, so there is no log directory to lock down.
	deps, _, sec := okDeps()
	cfg, values := resolve(t, "authTokensFile = '"+testTokenStore+"'\n")

	// ACT
	results, err := SecurePaths(deps, cfg, values)

	// ASSERT
	// An unset key must not arrive as an empty path: securing "" would resolve
	// to the working directory and apply a protected, inheriting
	// SYSTEM-and-Administrators list to whatever the operator ran from.
	require.NoError(t, err)
	require.Len(t, results, 2)

	for _, target := range sec.Targets {
		assert.NotEmpty(t, target.Path)
	}
}

func TestSecurePaths_ShouldPropagateAFailure(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, _, sec := okDeps()
	sec.Err = winsvc.ErrNotSecured

	cfg, values := resolve(t,
		"authTokensFile = '"+testTokenStore+"'\nlogFile = '"+testLogFile+"'\n")

	// ACT
	results, err := SecurePaths(deps, cfg, values)

	// ASSERT
	// Reporting partial coverage for a failed lockdown is how an operator
	// comes to believe a file is protected when it is not.
	require.ErrorIs(t, err, winsvc.ErrNotSecured)
	assert.Nil(t, results)
}

func TestSecurePaths_ShouldRefuseALockdownAimedAtAVolumeRoot(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// `logFile = C:\adapter.log` is the whole typo. SecurablesFor secures the
	// log file's DIRECTORY — the adapter creates the file at runtime and it
	// inherits — and the directory of a file at the volume root is the volume
	// root. CheckServicePaths passes it: the path is absolute.
	deps, _, sec := okDeps()
	cfg, values := resolve(t,
		"authTokensFile = '"+testTokenStore+"'\nlogFile = 'C:\\adapter.log'\n")

	// ACT
	_, err := SecurePaths(deps, cfg, values)

	// ASSERT
	// Refused, and nothing reached the lockdown: a protected, inheriting
	// SYSTEM-and-Administrators list applied to C:\ strips every other
	// application's access to its own files across the whole volume, and no
	// re-run undoes it.
	require.ErrorIs(t, err, winsvc.ErrProtectedLocation)
	assert.Empty(t, sec.Targets, "nothing may be secured once a target is refused")
}
