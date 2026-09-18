package setup

import (
	"context"
	"errors"
	"fmt"
)

// Condition is what a step's Check found.
//
// Three values rather than a boolean, because "not done" splits into two cases
// that must not be treated alike: one this run can fix, and one it must refuse
// until an operator says otherwise.
type Condition int

const (
	// Satisfied means the step's outcome is already in place. Apply is not
	// called.
	Satisfied Condition = iota

	// Pending means the step has work to do and may do it.
	Pending

	// Blocked means the step could do the work but will not without an
	// explicit instruction — a reinstall over a registration that differs, a
	// restart of a running service, an upgrade of a binary held open. Every
	// Blocked verdict names the flag that clears it, or it is a dead end
	// wearing a status.
	Blocked
)

// String renders the condition for the report.
func (c Condition) String() string {
	switch c {
	case Satisfied:
		return "satisfied"
	case Pending:
		return "pending"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// Verdict is one step's answer, with the sentence an operator reads.
type Verdict struct {
	// Condition is the finding.
	Condition Condition

	// Detail says what was found, in one line. It is not decoration: a report
	// of seven steps and three conditions tells an operator nothing about
	// WHICH registration differs or WHICH path is wrong.
	Detail string
}

// satisfied, pending and blocked build a Verdict with a formatted detail.
func satisfied(format string, args ...any) Verdict {
	return Verdict{Condition: Satisfied, Detail: fmt.Sprintf(format, args...)}
}

func pending(format string, args ...any) Verdict {
	return Verdict{Condition: Pending, Detail: fmt.Sprintf(format, args...)}
}

func blocked(format string, args ...any) Verdict {
	return Verdict{Condition: Blocked, Detail: fmt.Sprintf(format, args...)}
}

// Step is one unit of provisioning, split so that finding out never changes
// anything.
//
// The split is the whole design. A single "do it if needed" function computes
// the same answer twice — once to report and once to act — and the two drift
// the first time somebody adds a condition to one of them, with nothing
// failing when they do.
type Step interface {
	// Name identifies the step in the report. Short, lowercase, stable: it is
	// what an operator greps for.
	Name() string

	// Check reports the step's condition WITHOUT mutating anything.
	//
	// It reads the plan's projected state — what the earlier pending steps
	// will produce — rather than only the live host. A check that read only
	// the host would report "blocked: no config" on a fresh machine for the
	// config this very run is about to write, and every later step would
	// inherit the mistake.
	//
	// A Check may record what it projects on the plan, so later Checks can see
	// it. An error means the condition could not be determined, which is
	// different from Blocked and is reported as such.
	Check(ctx context.Context, p *Plan) (Verdict, error)

	// Apply performs the step.
	//
	// It re-verifies the real state first. Checks run against a projection and
	// against a host that may have changed since; Apply is the last moment
	// before a mutation and the only one that can be sure.
	Apply(ctx context.Context, p *Plan) error
}

// StepResult is what one step did across the whole run.
type StepResult struct {
	// Name is the step's name.
	Name string

	// Verdict is what Check found.
	Verdict Verdict

	// Applied reports whether Apply ran and succeeded.
	Applied bool

	// Err is the failure that stopped the run at this step, if any.
	Err error
}

// Result is the whole run, as data.
//
// As data, and not as printed lines, because the callers differ in what they
// can consume: the service subcommand prints, an MSI custom action reads an
// exit code, and a GUI would render something else again. Everything a caller
// needs to decide an exit code is here.
type Result struct {
	// Steps is every step, in order, whether or not it was applied.
	Steps []StepResult

	// DryRun reports whether anything was allowed to mutate.
	DryRun bool

	// Blocked lists the steps whose verdict was Blocked. A non-empty Blocked
	// is the exit-code-3 case and is not an error: the run did exactly what it
	// was asked and is saying what it will not do unasked.
	Blocked []StepResult

	// Health is what the verification step found, when it ran.
	Health HealthReport

	// Token is the bearer token this run minted, if it minted one. It reaches
	// the caller exactly once, because the store keeps only a hash and nothing
	// can recover it afterwards — and it is NOT on the Result of a run that
	// found a usable token already there, which never saw one.
	Token string

	// ConfigWritten reports whether this run created the config file, as
	// opposed to validating one that was already there.
	ConfigWritten bool
}

// HealthReport is what the verification step saw.
//
// Separate from a plain pass/fail because "installed and running, backend
// unhealthy" is its own outcome: it is what every developer machine and every
// CI runner answers, and automation must be able to tell it from a step that
// failed.
type HealthReport struct {
	// Checked reports whether verification ran at all.
	Checked bool

	// Reachable reports whether the adapter answered its health endpoint.
	Reachable bool

	// ComponentHealthy reports whether the named component was healthy. The
	// HTTP status cannot answer this: health answers 200 for healthy and
	// unhealthy alike, so this comes from the body.
	ComponentHealthy bool

	// Component is the component that was required, and Detail is what it
	// reported.
	Component string
	Detail    string

	// AuthProved reports whether an authenticated call reached a protected
	// route. The predicate is "any status other than 401" — a host with no
	// backend answers 502 or 504 there, and demanding 200 would fail every
	// dev machine while proving nothing extra.
	AuthProved bool

	// AuthSkipped and AuthSkipReason record a verification that could not be
	// made, rather than one that passed. Bearer auth turned off is the case:
	// minting into a store nothing reads and calling it proof is worse than
	// saying nothing.
	AuthSkipped    bool
	AuthSkipReason string
}

// Failed reports whether any step errored.
func (r Result) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != nil {
			return true
		}
	}

	return false
}

// Run checks every step, then applies the pending ones in order.
//
// Checks all first, deliberately. An operator running an elevated, mutating
// command against a production DHCP server gets to see the whole plan before
// any of it happens, and --dry-run is then the same pass with the applying
// half skipped. The cost is that Check must work against projected rather than
// live state; that cost is paid in Plan.
//
// The run stops at the first Apply failure, and at any Blocked step before
// applying anything at all. Half-provisioning a host and then reporting a
// refusal is the outcome worth avoiding: a Blocked verdict is a question, and
// the answer may be "not like that".
func Run(ctx context.Context, opts Options, deps Deps) (Result, error) {
	if err := opts.check(); err != nil {
		return Result{}, err
	}

	plan := newPlan(opts, deps)

	return runOver(ctx, opts, plan, plan.steps())
}

// runOver is Run with the step list supplied.
//
// Split out so the machinery — the ordering, the projection, the refusal to
// mutate around a Blocked verdict, where a failure is recorded — is testable
// against stub steps. A test of that machinery driven through the seven real
// steps would fail for reasons that belong to the config file's rules or the
// SCM's, which is the opposite of what it is trying to pin down.
func runOver(ctx context.Context, opts Options, plan *Plan, steps []Step) (Result, error) {
	result := Result{DryRun: opts.DryRun, Steps: make([]StepResult, 0, len(steps))}

	for _, step := range steps {
		verdict, err := step.Check(ctx, plan)
		if err != nil {
			result.Steps = append(result.Steps, StepResult{
				Name: step.Name(),
				Err:  fmt.Errorf("checking %s: %w", step.Name(), err),
			})

			return result, fmt.Errorf("checking %s: %w", step.Name(), err)
		}

		entry := StepResult{Name: step.Name(), Verdict: verdict}
		result.Steps = append(result.Steps, entry)

		if verdict.Condition == Blocked {
			result.Blocked = append(result.Blocked, entry)
		}
	}

	// A Blocked step stops the run before anything mutates, dry or not. The
	// alternative — apply what can be applied and report the block afterwards
	// — leaves a half-provisioned host whose operator now has to work out
	// which half.
	if opts.DryRun || len(result.Blocked) > 0 {
		plan.fill(&result)

		return result, nil
	}

	err := applyPending(ctx, steps, plan, &result)

	plan.fill(&result)

	return result, err
}

// applyPending runs each pending step's Apply in order, stopping at the first
// failure and recording it against the step that failed.
func applyPending(ctx context.Context, steps []Step, plan *Plan, result *Result) error {
	for i, step := range steps {
		if result.Steps[i].Verdict.Condition != Pending {
			continue
		}

		if err := step.Apply(ctx, plan); err != nil {
			result.Steps[i].Err = err

			return fmt.Errorf("%s: %w", step.Name(), err)
		}

		result.Steps[i].Applied = true
	}

	return nil
}

// ErrBlocked reports that a step will not proceed without an explicit
// instruction. It is carried on the Result rather than returned, so a caller
// can tell a refusal from a failure; this exists for callers that would rather
// match on an error.
var ErrBlocked = errors.New("setup: a step is blocked and needs an explicit flag")
