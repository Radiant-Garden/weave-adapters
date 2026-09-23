/*
Testing: options.go

Pending:

Tested:
  Options.check
    - TestOptionsCheck_ShouldAcceptACompleteSetOfOptions
    - TestOptionsCheck_ShouldRefuseEachMissingRequirement: each with the reason it is required.
    - TestOptionsCheck_ShouldReportEveryProblemAtOnce
    - TestOptionsCheck_ShouldValidateOnlyTheDefinitionFieldsATemplateOwns: BinPath is the install step's to set.
  Layout.check
    - TestLayoutCheck_ShouldRequireEveryPath
    - TestLayoutCheck_ShouldAcceptAWindowsAbsolutePathOnAnyHost
    - TestLayoutCheck_ShouldRefuseARelativePath
    - TestLayoutCheck_ShouldRefuseAVolumeRootOrASharedSystemDirectory
  Options.configPath
    - TestOptionsConfigPath_ShouldPreferTheCallersOverrideOverTheLayout
  Options.verifyDeadline / Options.now
    - TestOptionsDefaults_ShouldFillTheInjectableValues

Tested elsewhere:
  What the options are FOR, and that each one changes the run: reconcile_test.go
  for the machinery, and the step tests for the flags each step reads.

  The flags an operator actually types, and how they become these values:
  cmd/weave-adapter-dhcp-windows/setup_test.go.

Declined:
  A test that constructs Options and reads its fields back. It would restate
  the struct literal and fail only if the compiler had already failed.

Additional Remarks:
  Two of the four paths are DIRECTORIES this command provisions into, and the
  lockdown it applies to Dir is protected and inheriting — it replaces the
  inherited access list of that directory and everything created in it. So
  `--data-dir C:\ProgramData`, one path segment away from what an operator
  meant, would strip every other application's rights to its own files, needs
  only the Administrator they already have, and is not undone by re-running
  anything. That refusal is here rather than in the lockdown because this is
  where the operator's own spelling still exists.

  The layout rule accepts a path absolute under EITHER Windows' rules or the
  host's, and that is worth a test rather than a comment alone. Windows' rule
  is the one that finally decides — CheckServicePaths applies it to these same
  values once they are in the config file — and on Windows the host rule is
  narrower than it looks, since filepath.IsAbs rejects a drive-less "\x"
  there. So nothing is loosened where it matters, and accepting the host's rule
  is what keeps this package drivable from a developer machine, which is the
  entire reason its platform operations are injected in the first place.
*/

package setup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
)

func TestOptionsCheck_ShouldAcceptACompleteSetOfOptions(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.NoError(t, runOptions(t).check())
}

func TestOptionsCheck_ShouldRefuseEachMissingRequirement(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate  func(*Options)
		wantErr string
	}{
		"should refuse a missing spec": {
			mutate: func(o *Options) { o.Spec = nil }, wantErr: "Spec is required",
		},
		"should refuse a missing validator": {
			mutate: func(o *Options) { o.Validate = nil }, wantErr: "Validate is required",
		},
		"should refuse a missing binary path": {
			mutate: func(o *Options) { o.BinPath = "" }, wantErr: "BinPath is required",
		},
		"should refuse a missing token label": {
			mutate: func(o *Options) { o.TokenLabel = "" }, wantErr: "TokenLabel is required",
		},
		"should refuse a missing health component": {
			mutate: func(o *Options) { o.HealthComponent = "" }, wantErr: "HealthComponent is required",
		},
		"should refuse a missing service name": {
			mutate: func(o *Options) { o.Definition.Name = "" }, wantErr: "Definition.Name is required",
		},
		"should refuse a non-positive drain budget": {
			mutate: func(o *Options) { o.Definition.DrainBudget = 0 }, wantErr: "DrainBudget must be positive",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			opts := runOptions(t)
			tc.mutate(&opts)

			// ACT
			err := opts.check()

			// ASSERT
			// Each of these is a wiring mistake rather than an operator one,
			// and each is refused before anything is read or touched.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestOptionsCheck_ShouldReportEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := Options{}.check()

	// ASSERT
	// Joined, for the same reason the configuration errors are: fixing one
	// field only to be told about the next is the version that wastes an
	// afternoon.
	require.Error(t, err)

	wanted := []string{
		"Spec is required", "Validate is required", "BinPath is required",
		"TokenLabel is required", "HealthComponent is required", "Definition.Name is required",
	}

	for _, want := range wanted {
		assert.Contains(t, err.Error(), want)
	}
}

func TestOptionsCheck_ShouldValidateOnlyTheDefinitionFieldsATemplateOwns(t *testing.T) {
	t.Parallel()

	// ARRANGE — a template, which by definition has no BinPath yet.
	opts := runOptions(t)
	opts.Definition.BinPath = ""
	opts.Definition.Args = nil

	// ACT
	err := opts.check()

	// ASSERT
	// BinPath and Args are filled in by the install step from what the binary
	// step chose. Running Definition.Validate here would refuse every correct
	// caller for a field it is not their job to set — and the full check still
	// runs inside Manager.Install once the definition is complete.
	assert.NoError(t, err)
}

func TestLayoutCheck_ShouldRequireEveryPath(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*Layout){
		"should require a directory":   func(l *Layout) { l.Dir = "" },
		"should require a config path": func(l *Layout) { l.ConfigPath = "" },
		"should require a token store": func(l *Layout) { l.TokenStorePath = "" },
		"should require a log path":    func(l *Layout) { l.LogPath = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			layout := runOptions(t).Layout
			mutate(&layout)

			// ACT / ASSERT
			require.Error(t, layout.check())
		})
	}
}

func TestLayoutCheck_ShouldAcceptAWindowsAbsolutePathOnAnyHost(t *testing.T) {
	t.Parallel()

	// ARRANGE — the real production layout, which is not absolute under a
	// developer machine's own rules.
	//
	//nolint:gosec // G101: paths to a store, not credentials.
	layout := Layout{
		Dir:            `C:\ProgramData\weave-adapters`,
		ConfigPath:     `C:\ProgramData\weave-adapters\config.toml`,
		TokenStorePath: `C:\ProgramData\weave-adapters\tokens.toml`,
		LogPath:        `C:\ProgramData\weave-adapters\adapter.log`,
		BinDir:         `C:\Program Files\weave-adapters`,
	}

	// ACT / ASSERT
	// Windows' rule is the one that finally decides: CheckServicePaths applies
	// it to these same values once they are in the config file, so a layout
	// this check accepted and that check refused would be two rules
	// disagreeing about one path.
	require.NoError(t, layout.check())
	assert.True(t, config.IsAbsoluteServicePath(layout.Dir))
}

func TestLayoutCheck_ShouldRefuseARelativePath(t *testing.T) {
	t.Parallel()

	// ARRANGE
	layout := runOptions(t).Layout
	layout.ConfigPath = filepathRelative

	// ACT
	err := layout.check()

	// ASSERT
	// Under the SCM the working directory is C:\Windows\System32, so a
	// relative path here provisions one place and is read from another — and
	// the symptom is a service that fails at startup naming a file the
	// operator can see exists.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
	assert.Contains(t, err.Error(), config.ServiceWorkingDirectory)
}

// filepathRelative is the shape the layout rule exists to catch: neither
// Windows-absolute nor absolute on this host.
const filepathRelative = `weave-adapters\config.toml`

func TestOptionsConfigPath_ShouldPreferTheCallersOverrideOverTheLayout(t *testing.T) {
	t.Parallel()

	// ARRANGE
	opts := runOptions(t)

	// ACT / ASSERT
	assert.Equal(t, opts.Layout.ConfigPath, opts.configPath())

	opts.ConfigPath = `C:\elsewhere\theirs.toml`
	assert.Equal(t, `C:\elsewhere\theirs.toml`, opts.configPath())
	assert.Equal(t, opts.configPath(), opts.ConfigFilePath(),
		"the exported accessor must not re-derive the choice and get it different")
}

func TestOptionsDefaults_ShouldFillTheInjectableValues(t *testing.T) {
	t.Parallel()

	// ARRANGE
	bare := Options{}

	// ACT / ASSERT
	// A zero deadline must not mean "do not wait": the service is still coming
	// up when verification starts, so no wait at all would fail every correct
	// installation.
	assert.Equal(t, defaultVerifyDeadline, bare.verifyDeadline())
	assert.Positive(t, bare.verifyDeadline())

	assert.WithinDuration(t, time.Now(), bare.now(), time.Minute)

	fixed := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, fixed, Options{Now: func() time.Time { return fixed }}.now())

	assert.Equal(t, time.Second, Options{VerifyDeadline: time.Second}.verifyDeadline())
}

func TestLayoutCheck_ShouldRefuseAVolumeRootOrASharedSystemDirectory(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate func(*Layout)
		want   string
	}{
		"should refuse a shared system directory as the data directory": {
			// One missing path segment from C:\ProgramData\weave-adapters.
			mutate: func(l *Layout) { l.Dir = `C:\ProgramData` },
			want:   "Layout.Dir",
		},
		"should refuse a volume root as the data directory": {
			mutate: func(l *Layout) { l.Dir = `C:\` },
			want:   "Layout.Dir",
		},
		"should refuse a shared system directory as the binary directory": {
			mutate: func(l *Layout) { l.BinDir = `C:\Program Files` },
			want:   "Layout.BinDir",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			//nolint:gosec // G101: paths to a store, not credentials.
			layout := Layout{
				Dir:            `C:\ProgramData\weave-adapters`,
				ConfigPath:     `C:\ProgramData\weave-adapters\config.toml`,
				TokenStorePath: `C:\ProgramData\weave-adapters\tokens.toml`,
				LogPath:        `C:\ProgramData\weave-adapters\adapter.log`,
				BinDir:         `C:\Program Files\weave-adapters`,
			}
			tc.mutate(&layout)

			// ACT
			err := layout.check()

			// ASSERT
			// The lockdown this command applies is protected and inheriting,
			// so it does not add to the directory's access list — it replaces
			// it, for the whole subtree. An administrator can do this, which
			// is exactly why it has to be refused before it is done.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "shared system directory")
		})
	}
}
