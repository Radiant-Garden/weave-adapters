/*
Testing: reconcile.go

Pending:

Tested:
  Run
    - TestRun_ShouldCheckEverythingBeforeApplyingAnything: the check pass is complete before the first mutation.
    - TestRun_ShouldReportAPlanForAFreshHostWithoutTouchingIt: --dry-run mutates nothing and reports every step.
    - TestRun_ShouldCheckAgainstProjectedStateRatherThanTheLiveHost: the row-13 rule, from the outside.
    - TestRun_ShouldApplyOnlyThePendingSteps: a satisfied step's Apply never runs.
    - TestRun_ShouldStopAtTheFirstFailureAndRecordIt
    - TestRun_ShouldChangeNothingWhenAnyStepIsBlocked: a refusal is answered before a half-provisioned host exists.
    - TestRun_ShouldReportACheckThatCouldNotBeDetermined: an errored check is exit 1, not a blocked verdict.
    - TestRun_ShouldRefuseIncompleteOptions
    - TestRun_ShouldBeIdempotentAcrossTwoRealRuns: the whole point of the reconciler, driven through the real steps.
  Condition.String
    - TestConditionString_ShouldNameEveryCondition
  Result.Failed
    - TestResultFailed_ShouldReportWhetherAnyStepErrored

Tested elsewhere:
  What each real step decides: provision_test.go, deploy_test.go,
  verify_test.go. The steps here are stubs on purpose — this file is about the
  machinery, and a test of Run that depended on the config step's rules would
  fail for reasons that have nothing to do with ordering.

  The one exception is the idempotency test, which drives the REAL steps end to
  end. Idempotency is not a property of any single step and is the promise the
  whole command makes; stubs cannot break it, and the bug that motivated the
  test — the plan comparing a raw --config against the absolute one Install
  registers — lived in the seam between two of them.

  The exit codes the Result maps onto, and the operator-facing rendering of a
  plan: cmd/weave-adapter-dhcp-windows/setup_test.go.

Declined:
  Asserting the wording of a Verdict.Detail built by the satisfied/pending/
  blocked helpers. They are fmt.Sprintf; the details that carry meaning are
  asserted in the step that produces them.

Additional Remarks:
  The stub steps record the ORDER of every Check and Apply into one shared
  slice. That is deliberate: the property worth pinning is not "checks happen"
  and "applies happen" but that ALL the checks precede ANY of the applies. A
  per-step counter could not tell the difference, and the interleaved
  alternative — check one, apply one — is a design that was considered and
  rejected, so nothing else in the code says it was.
*/

package setup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// journal records what the stub steps did, in order, across a whole run.
type journal struct{ entries []string }

func (j *journal) record(entry string) { j.entries = append(j.entries, entry) }

// stubStep is a Step whose verdict and failures a test dictates.
type stubStep struct {
	name     string
	verdict  Verdict
	checkErr error
	applyErr error
	log      *journal

	// project is called during Check, so a test can put something on the plan
	// that a later step's Check must be able to see.
	project func(*Plan)
}

func (s *stubStep) Name() string { return s.name }

func (s *stubStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	s.log.record("check:" + s.name)

	if s.project != nil {
		s.project(p)
	}

	return s.verdict, s.checkErr
}

func (s *stubStep) Apply(_ context.Context, _ *Plan) error {
	s.log.record("apply:" + s.name)

	return s.applyErr
}

// runStubs drives Run over the given steps, bypassing the real step list.
//
// The plan's steps() is what a real run uses; swapping the list here is the
// only way to exercise the machinery without also exercising seven sets of
// filesystem and SCM rules.
func runStubs(t *testing.T, steps []Step) (Result, error) {
	t.Helper()

	opts := runOptions(t)
	deps, _, _ := okDeps()

	plan := newPlan(opts, deps)

	return runOver(context.Background(), opts, plan, steps)
}

func TestRun_ShouldCheckEverythingBeforeApplyingAnything(t *testing.T) {
	t.Parallel()

	// ARRANGE
	log := &journal{}
	steps := []Step{
		&stubStep{name: "first", verdict: pending("x"), log: log},
		&stubStep{name: "second", verdict: pending("x"), log: log},
		&stubStep{name: "third", verdict: pending("x"), log: log},
	}

	// ACT
	_, err := runStubs(t, steps)

	// ASSERT
	// All the checks, then all the applies. An operator running an elevated
	// mutating command against a production DHCP server sees the whole plan
	// before any of it happens — and it is why Check has to work against a
	// projection rather than only against the live host.
	require.NoError(t, err)
	assert.Equal(t, []string{
		"check:first", "check:second", "check:third",
		"apply:first", "apply:second", "apply:third",
	}, log.entries)
}

func TestRun_ShouldApplyOnlyThePendingSteps(t *testing.T) {
	t.Parallel()

	// ARRANGE
	log := &journal{}
	steps := []Step{
		&stubStep{name: "done", verdict: satisfied("already there"), log: log},
		&stubStep{name: "todo", verdict: pending("not yet"), log: log},
	}

	// ACT
	result, err := runStubs(t, steps)

	// ASSERT
	require.NoError(t, err)
	assert.NotContains(t, log.entries, "apply:done", "a satisfied step was applied anyway")
	assert.Contains(t, log.entries, "apply:todo")

	require.Len(t, result.Steps, 2)
	assert.False(t, result.Steps[0].Applied)
	assert.True(t, result.Steps[1].Applied)
}

func TestRun_ShouldCheckAgainstProjectedStateRatherThanTheLiveHost(t *testing.T) {
	t.Parallel()

	// ARRANGE — the first step projects what it WILL produce; the second reads
	// it during its own Check.
	log := &journal{}

	var seen string

	steps := []Step{
		&stubStep{
			name: "producer", verdict: pending("will write it"), log: log,
			project: func(p *Plan) { p.ConfigPath = "projected.toml" },
		},
		&stubStep{
			name: "consumer", verdict: pending("x"), log: log,
			project: func(p *Plan) { seen = p.ConfigPath },
		},
	}

	// ACT
	_, err := runStubs(t, steps)

	// ASSERT
	// Without this a dry run on a fresh host reports "blocked: no config" for
	// the config the previous step is about to write, and every later step
	// inherits the mistake.
	require.NoError(t, err)
	assert.Equal(t, "projected.toml", seen,
		"the second step's Check could not see what the first will produce")
}

func TestRun_ShouldReportAPlanForAFreshHostWithoutTouchingIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	log := &journal{}
	steps := []Step{
		&stubStep{name: "first", verdict: pending("would do it"), log: log},
		&stubStep{name: "second", verdict: pending("would do it too"), log: log},
	}

	opts := runOptions(t)
	opts.DryRun = true

	deps, m, sec := okDeps()

	// ACT
	result, err := runOver(context.Background(), opts, newPlan(opts, deps), steps)

	// ASSERT
	require.NoError(t, err)
	assert.True(t, result.DryRun)

	for _, entry := range log.entries {
		assert.NotContains(t, entry, "apply:", "a dry run applied %s", entry)
	}

	// Nothing that mutates, on any seam. A read-only SCM query is fine; a
	// lockdown or a registration is not.
	assert.Empty(t, m.Installed)
	assert.Empty(t, sec.Targets)

	// And the plan is still reported in full, which is the point of the flag.
	require.Len(t, result.Steps, 2)
	assert.Equal(t, "would do it", result.Steps[0].Verdict.Detail)
}

func TestRun_ShouldChangeNothingWhenAnyStepIsBlocked(t *testing.T) {
	t.Parallel()

	// ARRANGE — the blocked step is LAST, so a run that applied as it went
	// would already have mutated before reaching it.
	log := &journal{}
	steps := []Step{
		&stubStep{name: "first", verdict: pending("would do it"), log: log},
		&stubStep{name: "stuck", verdict: blocked("needs --reinstall"), log: log},
	}

	// ACT
	result, err := runStubs(t, steps)

	// ASSERT
	// A Blocked verdict is a question, and the answer may be "not like that".
	// Applying the earlier half and reporting the refusal afterwards leaves an
	// operator working out which half happened.
	require.NoError(t, err, "a refusal is an outcome, not a failure")

	for _, entry := range log.entries {
		assert.NotContains(t, entry, "apply:", "%s ran despite a blocked step", entry)
	}

	require.Len(t, result.Blocked, 1)
	assert.Equal(t, "stuck", result.Blocked[0].Name)
	assert.Equal(t, "needs --reinstall", result.Blocked[0].Verdict.Detail)
}

func TestRun_ShouldStopAtTheFirstFailureAndRecordIt(t *testing.T) {
	t.Parallel()

	// ARRANGE
	log := &journal{}
	boom := errors.New("the disk is full")

	steps := []Step{
		&stubStep{name: "first", verdict: pending("x"), log: log},
		&stubStep{name: "breaks", verdict: pending("x"), applyErr: boom, log: log},
		&stubStep{name: "never", verdict: pending("x"), log: log},
	}

	// ACT
	result, err := runStubs(t, steps)

	// ASSERT
	// Stopping matters more than finishing: the steps are ordered because each
	// depends on the last, so carrying on after a failure provisions against
	// state that was never produced.
	require.ErrorIs(t, err, boom)
	assert.NotContains(t, log.entries, "apply:never")

	require.Len(t, result.Steps, 3)
	assert.True(t, result.Steps[0].Applied)
	require.Error(t, result.Steps[1].Err)
	assert.False(t, result.Steps[1].Applied)
	require.NoError(t, result.Steps[2].Err, "a step that never ran must not be reported as failed")

	assert.True(t, result.Failed())
}

func TestRun_ShouldReportACheckThatCouldNotBeDetermined(t *testing.T) {
	t.Parallel()

	// ARRANGE
	log := &journal{}
	boom := errors.New("the SCM could not be reached")

	steps := []Step{
		&stubStep{name: "first", verdict: pending("x"), log: log},
		&stubStep{name: "unknowable", checkErr: boom, log: log},
		&stubStep{name: "never", verdict: pending("x"), log: log},
	}

	// ACT
	result, err := runStubs(t, steps)

	// ASSERT
	// Distinct from Blocked. Blocked means "this run will not, without a
	// flag"; an errored check means nobody knows what the state is, and
	// provisioning against an unknown state is the thing to refuse outright.
	require.ErrorIs(t, err, boom)
	assert.NotContains(t, log.entries, "check:never", "checking continued past a check that could not answer")
	assert.Empty(t, result.Blocked)
	assert.True(t, result.Failed())
}

func TestRun_ShouldRefuseIncompleteOptions(t *testing.T) {
	t.Parallel()

	// ARRANGE
	deps, m, sec := okDeps()

	// ACT
	_, err := Run(context.Background(), Options{}, deps)

	// ASSERT
	// Before anything is looked at, let alone touched.
	require.Error(t, err)
	assert.Empty(t, m.Calls)
	assert.Empty(t, sec.Targets)
}

func TestConditionString_ShouldNameEveryCondition(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	// The strings reach an operator's screen, so an unnamed condition would
	// print as a bare integer in the column they scan.
	assert.Equal(t, "satisfied", Satisfied.String())
	assert.Equal(t, "pending", Pending.String())
	assert.Equal(t, "blocked", Blocked.String())
	assert.Equal(t, "unknown", Condition(99).String())
}

func TestResultFailed_ShouldReportWhetherAnyStepErrored(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		steps []StepResult
		want  bool
	}{
		"should report no failure for a clean run": {
			steps: []StepResult{{Name: "a"}, {Name: "b", Applied: true}},
		},
		"should report a failure anywhere in the run": {
			steps: []StepResult{{Name: "a"}, {Name: "b", Err: errors.New("x")}},
			want:  true,
		},
		"should report no failure for an empty run": {},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tc.want, Result{Steps: tc.steps}.Failed())
		})
	}
}

func TestRun_ShouldBeIdempotentAcrossTwoRealRuns(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// An operator's own config, at an absolute path spelled with a "." segment
	// — which filepath.Abs cleans and Install therefore registers cleaned. That
	// is C2's mechanism in the shape that is still allowed: the plan used to
	// hold the raw string and compare it against the cleaned one the SCM
	// reports, so the run was Blocked for ever.
	configPath := writeConfig(t, "")
	dir := filepath.Dir(configPath)

	opts := runOptions(t)
	// Concatenated, not filepath.Join: Join CLEANS, and the whole point is a
	// spelling that filepath.Abs will change.
	opts.ConfigPath = dir + string(filepath.Separator) + "." + string(filepath.Separator) + "config.toml"
	opts.Provisioned = nil

	deps, m, _ := okDeps()

	// The registration steps only. The token step would open the token store
	// and the verify step would poll a listener, neither of which this is
	// about — and the token store's path is Windows-shaped, so opening it on a
	// developer host creates a literal `C:\…` directory beside the package.
	steps := []Step{configStep{}, binaryStep{}, installStep{}}

	// ACT — the first run.
	first, err := runOver(context.Background(), opts, newPlan(opts, deps), steps)
	require.NoError(t, err)
	require.Empty(t, first.Blocked)
	require.Len(t, m.Installed, 1, "the first run must register the service")

	// The SCM now reports what that run ACTUALLY registered — not what the plan
	// intended, which is the distinction the old fixture lost.
	registered := m.Installed[0]
	m.Reported = winsvc.ServiceStatus{
		Name:      registered.Name,
		Installed: true,
		BinPath:   registered.BinPath,
		Command:   append([]string{registered.BinPath}, registered.Args...),
		State:     winsvc.StateRunning,
	}

	// ACT — the second run, same options, against the host the first one left.
	second, err := runOver(context.Background(), opts, newPlan(opts, deps), steps)

	// ASSERT
	// Nothing blocked and nothing registered twice. A run whose ConfigPath did
	// not match what Install registers reported
	//
	//	… is registered but runs [… --config /abs/config.toml],
	//	not [… --config ./config.toml]; re-run with --reinstall
	//
	// for ever — and --reinstall re-registered the same absolute form and
	// mismatched again next time, so the invocation shape was permanently
	// non-idempotent and permanently exit 3.
	require.NoError(t, err)
	assert.Empty(t, second.Blocked, "a re-run against an unchanged host must find nothing to refuse")
	assert.Len(t, m.Installed, 1, "the second run registered the service again")

	for _, step := range second.Steps {
		assert.Equal(t, Satisfied, step.Verdict.Condition, "step %s", step.Name)
	}
}

func TestRun_ShouldRefuseARelativeConfigOverride(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)
	opts.ConfigPath = "config.toml"

	deps, m, sec := okDeps()

	// ACT
	_, err := Run(context.Background(), opts, deps)

	// ASSERT
	// It becomes the SERVICE's --config argument, and under the SCM the working
	// directory is C:\Windows\System32 — so it resolves at provisioning time
	// against the operator's shell and at every boot against System32. Refused
	// before anything is touched, rather than provisioned into a run that can
	// never reach Satisfied.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Options.ConfigPath must be absolute")
	assert.Empty(t, m.Calls, "the SCM was contacted while refusing")
	assert.Empty(t, sec.Targets)
}
