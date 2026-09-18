/*
Testing: plan.go

Pending:

Tested:
  Plan.steps
    - TestPlanSteps_ShouldRunInTheOnlyOrderThatIsCorrect: the directory first, verification last.
  Plan.fill
    - TestPlanFill_ShouldCarryWhatTheRunProducedOntoTheResult: including the token, which exists nowhere else.
  Plan.wantsRestart
    - TestPlanWantsRestart_ShouldKeepTheFirstReason
  newPlan
    - TestNewPlan_ShouldStartFromTheOptionsItWasGiven

Tested elsewhere:
  Every field's meaning, and that each step reads and writes the ones it
  should: provision_test.go, deploy_test.go, verify_test.go.

  That a later step's Check can see what an earlier one projected — the whole
  reason the Plan is shared: reconcile_test.go.

Declined:
  Asserting the unexported fields directly beyond restartWanted. They are
  written and read by steps in the same package, and every one of them is
  covered through the behaviour it produces.

Additional Remarks:
  The step order is asserted by NAME rather than by type, because the property
  worth pinning is the sequence an operator sees and depends on, not the
  identity of the structs implementing it.

  The order is not arbitrary and both ends matter. The directory comes first
  because locking it is what makes every file written afterwards inherit the
  locked-down entries — on a default C:\ProgramData ACL, a file created by an
  elevated process inherits read for local Users, and the first file written
  carries identity.namespaceKey. Verification comes last and is not optional: a
  run that registered a service and never looked at whether it came up has
  reported success for the half of the job an operator can watch fail.
*/

package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanSteps_ShouldRunInTheOnlyOrderThatIsCorrect(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	// ACT
	steps := newPlan(opts, deps).steps()

	// ASSERT
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name())
	}

	assert.Equal(t, []string{
		"directory", "config", "token", "binary", "install", "start", "verify",
	}, names)

	// Spelled twice on purpose: the two ends are the ones with a security or
	// a reporting consequence, and a reordering that broke either would still
	// satisfy a test that only counted the steps.
	assert.Equal(t, "directory", names[0], "a file written before the lockdown is briefly world-readable")
	assert.Equal(t, "verify", names[len(names)-1], "a run that never looked cannot report success")
}

func TestPlanFill_ShouldCarryWhatTheRunProducedOntoTheResult(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	p := newPlan(opts, deps)
	p.Token = "wadapt_the-only-copy-there-will-ever-be"
	p.ConfigExists = false
	p.health = HealthReport{Checked: true, Reachable: true, ComponentHealthy: true}

	// ACT
	var result Result

	p.fill(&result)

	// ASSERT
	// One place, called on every exit from Run, so a path that returns early
	// cannot be the one that forgets to report the token it just minted —
	// which would lose the only copy that will ever exist.
	assert.Equal(t, p.Token, result.Token)
	assert.True(t, result.ConfigWritten)
	assert.True(t, result.Health.ComponentHealthy)
}

func TestPlanFill_ShouldReportAnExistingConfigAsNotWritten(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	p := newPlan(opts, deps)
	p.ConfigExists = true

	// ACT
	var result Result

	p.fill(&result)

	// ASSERT
	// The caller prints "provisioned into …" off this, and printing it for a
	// file it validated and left alone would claim a write that never
	// happened.
	assert.False(t, result.ConfigWritten)
}

func TestPlanWantsRestart_ShouldKeepTheFirstReason(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()
	p := newPlan(opts, deps)

	// ACT
	p.wantsRestart("a token was minted")
	p.wantsRestart("the binary was replaced")

	// ASSERT
	// The first, because it is the one the operator will be shown and there is
	// one line to show it in. Either reason justifies the same restart, so
	// overwriting would only make the message depend on step order.
	assert.Equal(t, "a token was minted", p.restartWanted)
}

func TestNewPlan_ShouldStartFromTheOptionsItWasGiven(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	deps, _, _ := okDeps()

	// ACT
	p := newPlan(opts, deps)

	// ASSERT
	// The starting state, before any Check has projected anything: the config
	// this run will use and the binary it was started from.
	assert.Equal(t, opts.configPath(), p.ConfigPath)
	assert.Equal(t, opts.BinPath, p.BinPath)
	assert.Empty(t, p.Token)
	assert.False(t, p.ConfigExists)
	require.Nil(t, p.Values, "a plan must not claim resolved values before the config step runs")
}
