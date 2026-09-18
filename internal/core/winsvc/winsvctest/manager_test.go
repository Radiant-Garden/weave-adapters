/*
Testing: manager.go

Pending:

Tested:
  Manager
    - TestManager_ShouldSatisfyTheManagerInterface: the conformance the whole double rests on.
    - TestManager_ShouldRecordEveryCallInOrder: Calls is what refusal tests assert against.
    - TestManager_ShouldNotRecordADefinitionWhenInstallFails: a failed install registered nothing.
  Securer
    - TestSecurer_ShouldReportEveryTargetApplied: the default, and the targets are recorded.
    - TestSecurer_ShouldReportTargetsSkippedWhenAsked: the not-yet-created shape a test has to opt into.

Tested elsewhere:
  What these doubles are FOR: the service subcommand's tests drive them through
  runService, and that is where the behaviour they enable is asserted.

  The real SCM operations, against a live Service Control Manager: task
  service-gate. internal/core/winsvc/service_windows.go has no unit coverage by
  design — see its own doc block.

Declined:
  Exhaustively asserting each recorded field. A double that recorded the wrong
  definition would fail the tests that consume it, loudly and by name; covering
  it twice here would only add a second place to update.

Additional Remarks:
  Testing a test double looks circular, and mostly is — what earns this file is
  the conformance assertion and the two behaviours a consumer cannot see: that
  a failed Install records the call but not the definition, and that Skip is
  opt-in. Both encode a decision. Getting either backwards would make a
  consumer's test pass while asserting the opposite of what it claims.
*/

package winsvctest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

func TestManager_ShouldSatisfyTheManagerInterface(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	var m winsvc.Manager = &Manager{}

	// ASSERT
	require.NotNil(t, m)
	assert.NoError(t, m.Close())
}

func TestManager_ShouldRecordEveryCallInOrder(t *testing.T) {
	t.Parallel()

	// ARRANGE
	m := &Manager{}
	def := winsvc.Definition{Name: "wadapt-test", BinPath: `C:\x\a.exe`}

	// ACT
	require.NoError(t, m.Install(def))
	require.NoError(t, m.Start("wadapt-test"))

	_, err := m.Status("wadapt-test")
	require.NoError(t, err)

	require.NoError(t, m.Stop("wadapt-test"))
	require.NoError(t, m.Uninstall("wadapt-test"))
	require.NoError(t, m.Close())

	// ASSERT
	// Order matters to the consumers: a refusal that still reached the SCM has
	// already done the thing it was refusing, and Calls is how that is caught.
	assert.Equal(t, []string{"install", "start", "status", "stop", "uninstall"}, m.Calls)
	assert.Equal(t, []winsvc.Definition{def}, m.Installed)
	assert.True(t, m.Closed)
}

func TestManager_ShouldNotRecordADefinitionWhenInstallFails(t *testing.T) {
	t.Parallel()

	// ARRANGE
	sentinel := errors.New("already installed")
	m := &Manager{InstallErr: sentinel}

	// ACT
	err := m.Install(winsvc.Definition{Name: "wadapt-test"})

	// ASSERT
	// The call happened, the registration did not — which is what a consumer
	// asserting "the SCM was contacted and refused" needs to be able to tell
	// apart from "nothing was attempted".
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, []string{"install"}, m.Calls)
	assert.Empty(t, m.Installed)
}

func TestSecurer_ShouldReportEveryTargetApplied(t *testing.T) {
	t.Parallel()

	// ARRANGE
	s := &Securer{}
	targets := []winsvc.Securable{
		{Path: `C:\x\config.toml`, Kind: winsvc.SecurableFile, Why: "the config file"},
		{Path: `C:\x\logs`, Kind: winsvc.SecurableDirectory, Why: "the log directory"},
	}

	// ACT
	results, err := s.Secure(targets)

	// ASSERT
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, targets, s.Targets)

	for _, r := range results {
		assert.True(t, r.Applied, "%s was reported as skipped without Skip being set", r.Target.Path)
	}
}

func TestSecurer_ShouldReportTargetsSkippedWhenAsked(t *testing.T) {
	t.Parallel()

	// ARRANGE — the shape of a target that does not exist yet, which the token
	// store legitimately is before the first mint.
	s := &Securer{Skip: true}

	// ACT
	results, err := s.Secure([]winsvc.Securable{{Path: `C:\x\tokens.toml`, Kind: winsvc.SecurableFile}})

	// ASSERT
	// Opt-in, so a test that does not ask for it cannot accidentally assert
	// the skipped-target message against a target that was in fact secured.
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, results[0].Applied)
}
