package setup

import (
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// Plan is the state every step shares, and the reason the steps are one
// sequence rather than a list of independent values.
//
// It carries two things that cannot be separated cleanly:
//
// The PROJECTION. Checks run before anything is applied, so a step's Check has
// to see what the earlier pending steps will produce rather than only what is
// on the host. The config step's Check renders and resolves the file it is
// about to write, without writing it, and puts the result here — so the token,
// install and verify steps can check against a configuration that does not
// exist yet. Without this, a dry run on a fresh host reports "no config" for
// the config the very next step would create, and every later step inherits
// the mistake.
//
// The HANDOVER. Steps produce values later steps need: the binary step chooses
// where the executable will live, the mint step produces the token verification
// needs, the config step produces the values everything reads.
//
// Fields are written by Check with projected values and overwritten by Apply
// with real ones. That is the point, not a hazard: a step that was Satisfied
// never applies, and its Check has already put the real value here.
type Plan struct {
	opts Options
	deps Deps

	// ConfigPath is the config file the service will be started with.
	ConfigPath string

	// ConfigExists reports whether that file was already on disk when this run
	// began. It is what makes "an existing config is never rewritten" a fact
	// rather than an intention.
	ConfigExists bool

	// Values is the resolved configuration: the real one when the file already
	// exists, the projected one when this run is about to write it.
	Values *config.Values

	// BinPath is the executable that will be registered — the copy
	// destination, or the source when nothing is copied.
	BinPath string

	// Token is what the mint step produced. Empty when nothing was minted,
	// which includes every run where a usable token was already there.
	Token string

	// Definition is what the install step will register, filled in by its
	// Check so the start and verify steps can name the service.
	Definition winsvc.Definition

	// Secured is every lockdown target this run covered.
	Secured []winsvc.SecureResult

	// health is what verification found. Unexported because it reaches the
	// caller through Result, which is the shape a caller is meant to read.
	health HealthReport

	// restartWanted records that something upstream needs the service
	// restarted before it takes effect — a freshly minted token, which
	// buildAuth reads only at startup, or a replaced binary.
	restartWanted string
}

// newPlan builds the starting state for a run.
func newPlan(opts Options, deps Deps) *Plan {
	return &Plan{
		opts:       opts,
		deps:       deps,
		ConfigPath: opts.configPath(),
		BinPath:    opts.BinPath,
	}
}

// steps returns the provisioning sequence, in the only order it is correct in.
//
// The directory comes first because it is the lockdown: on a default
// C:\ProgramData ACL a file created by an elevated process inherits read for
// local Users, so anything written before the directory is locked is
// world-readable in the window between — and the first thing written carries
// identity.namespaceKey.
//
// Verification comes last and is not optional. A run that registered a service
// and did not look at whether it came up has reported success for the half of
// the job an operator can see fail.
func (p *Plan) steps() []Step {
	return []Step{
		directoryStep{},
		configStep{},
		tokenStep{},
		binaryStep{},
		installStep{},
		startStep{},
		verifyStep{},
	}
}

// fill copies what the run produced onto the Result.
//
// One place, called on every exit from Run, so a path that returns early
// cannot be the one that forgets to report the token it just minted — which
// would lose the only copy that will ever exist.
func (p *Plan) fill(result *Result) {
	result.Health = p.health
	result.Token = p.Token
	result.ConfigWritten = !p.ConfigExists
}

// wantsRestart records why a restart is needed, keeping the first reason.
func (p *Plan) wantsRestart(reason string) {
	if p.restartWanted == "" {
		p.restartWanted = reason
	}
}
