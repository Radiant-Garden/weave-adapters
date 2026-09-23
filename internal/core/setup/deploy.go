package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// ---------------------------------------------------------------------------
// binary
// ---------------------------------------------------------------------------

// binaryStep copies the executable to the directory the service will run from.
//
// It exists because `install` refuses a binary whose own directory an
// unprivileged account can write — which is every Downloads folder, and which
// is not pedantry: a LocalSystem service launched from such a folder is a full
// escalation, since replacing the exe makes Windows run it as SYSTEM at the
// next start. Copying is how a run that started from a download still ends
// with a service registered somewhere defensible.
type binaryStep struct{}

func (binaryStep) Name() string { return "binary" }

func (s binaryStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	source, err := filepath.Abs(p.opts.BinPath)
	if err != nil {
		return Verdict{}, fmt.Errorf("resolving %q: %w", p.opts.BinPath, err)
	}

	if p.opts.NoCopy {
		p.BinPath = source

		return satisfied("registering %s where it is", source), nil
	}

	dest := filepath.Join(p.opts.Layout.BinDir, filepath.Base(source))

	// Projected, so the install step's Check compares against the path that
	// WILL be registered rather than the one this process happens to be
	// running from.
	p.BinPath = dest

	if sameFile(source, dest) {
		return satisfied("%s is already the installed binary", dest), nil
	}

	destSum, err := fileSum(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return pending("copy %s to %s", source, dest), nil
	}

	if err != nil {
		return Verdict{}, err
	}

	sourceSum, err := fileSum(source)
	if err != nil {
		return Verdict{}, err
	}

	if sourceSum == destSum {
		return satisfied("%s is already this exact binary", dest), nil
	}

	// An upgrade. A RUNNING service holds its own executable open, so
	// overwriting it fails with a sharing violation until it is stopped — the
	// deferred release pipeline showing through, and exactly the kind of thing
	// that should be a named refusal rather than a copy error.
	//
	// Asked, not assumed. This used to report "the service holds it open" for
	// any hash mismatch, whether or not a service was installed or running —
	// and a stale executable left behind by `service uninstall`, or after a
	// manual stop, is the common case. The message was simply false there, and
	// --restart would have bounced a service that was not up to be bounced.
	running, err := s.serviceRunning(p)
	if err != nil {
		return Verdict{}, err
	}

	if !running {
		return pending("replace %s", dest), nil
	}

	if p.opts.Restart {
		p.wantsRestart("the binary is being replaced")

		return pending("stop the service and replace %s", dest), nil
	}

	return blocked("%s differs from this binary and %s is running, which holds it open; "+
		"re-run with --restart to stop, replace and start it", dest, p.opts.Definition.Name), nil
}

// serviceRunning reports whether the service is installed and up, which is the
// only state in which the destination binary cannot simply be replaced.
//
// stopForReplace already tolerates every other state — an absent registration
// and an already-stopped service are both no-ops there — so this is the only
// place that needed to learn the difference.
func (binaryStep) serviceRunning(p *Plan) (bool, error) {
	m, err := p.deps.NewManager()
	if err != nil {
		return false, err
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(p.opts.Definition.Name)
	if err != nil {
		return false, fmt.Errorf("reading the state of %s: %w", p.opts.Definition.Name, err)
	}

	return status.Installed && status.State == winsvc.StateRunning, nil
}

func (s binaryStep) Apply(_ context.Context, p *Plan) error {
	source, err := filepath.Abs(p.opts.BinPath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", p.opts.BinPath, err)
	}

	dest := p.BinPath

	if p.opts.NoCopy || sameFile(source, dest) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), provisionedDirMode); err != nil {
		return fmt.Errorf("creating %q: %w", filepath.Dir(dest), err)
	}

	// An upgrade needs the old service stopped first: Windows refuses to
	// replace a running service's image.
	if err := s.stopForReplace(p, dest); err != nil {
		return err
	}

	if err := copyFile(source, dest); err != nil {
		return err
	}

	// Verified after the rename, which is the only check that a torn copy to a
	// slow disk is not what gets registered. Cheap, and the failure it catches
	// is a service that will not start for a reason nothing else would name.
	sourceSum, err := fileSum(source)
	if err != nil {
		return err
	}

	destSum, err := fileSum(dest)
	if err != nil {
		return err
	}

	if sourceSum != destSum {
		return fmt.Errorf("the copy of %s at %s does not match the source; it was not registered", source, dest)
	}

	return nil
}

// stopForReplace stops the service when an existing binary is about to be
// overwritten. A destination that does not exist yet needs nothing stopped.
func (binaryStep) stopForReplace(p *Plan, dest string) error {
	if _, err := os.Stat(dest); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	m, err := p.deps.NewManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	if err := m.Stop(p.opts.Definition.Name); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
		return fmt.Errorf("stopping %s before replacing its binary: %w", p.opts.Definition.Name, err)
	}

	return nil
}

// copyFile writes source to dest through a temp file in the destination
// directory, then renames it into place.
//
// Through a temp file so a half-written executable is never the thing that
// gets registered: a rename within a directory is atomic, a partial write is
// not. The fsync is what makes that true of a power loss rather than only of
// a killed process.
func copyFile(source, dest string) error {
	in, err := os.Open(source) //nolint:gosec // the executable this run was started from
	if err != nil {
		return fmt.Errorf("opening %q: %w", source, err)
	}

	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".adapter-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp file in %q: %w", filepath.Dir(dest), err)
	}

	tmpName := tmp.Name()

	// Best effort: after a successful rename there is nothing to remove.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("copying %q to %q: %w", source, tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("syncing %q: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmpName, err)
	}

	// Owner-only, and executable — a copied binary that cannot be executed is
	// a service that will not start. Windows ignores the bit entirely and
	// decides by the destination directory's ACL, which install refuses to
	// proceed without; the mode is what keeps the file sane everywhere else.
	//
	//nolint:gosec // G302: 0600 would make the copy non-executable, which is the whole point of copying it.
	if err := os.Chmod(tmpName, 0o700); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("replacing %q: %w", dest, err)
	}

	return nil
}

// fileSum returns a file's SHA-256, hex-encoded.
func fileSum(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a path this run resolved itself
	if err != nil {
		// Passed through unwrapped so errors.Is(err, fs.ErrNotExist) still
		// answers at the call site, where "not there yet" is a normal case.
		return "", err
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()

	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %q: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// sameFile reports whether two paths name the same file, following the
// platform's own idea of sameness rather than comparing strings — which would
// answer wrongly for a differing case on Windows or a symlinked directory.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}

	bi, err := os.Stat(b)
	if err != nil {
		return false
	}

	return os.SameFile(ai, bi)
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// installStep registers the service, through exactly the same function
// `service install` calls.
//
// The same function, not a similar one. That is what makes "setup reuses the
// install path" structural: the binary-directory refusal, the would-it-start
// validation and the lockdown are not re-implemented here more kindly, and a
// change to any of them reaches both callers at once.
type installStep struct{}

func (installStep) Name() string { return "install" }

func (s installStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	def := p.opts.Definition
	def.BinPath = p.BinPath
	def.Args = []string{configFlag, p.ConfigPath}
	p.Definition = def

	m, err := p.deps.NewManager()
	if err != nil {
		return Verdict{}, err
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(def.Name)
	if err != nil {
		return Verdict{}, fmt.Errorf("reading the state of %s: %w", def.Name, err)
	}

	if !status.Installed {
		return pending("register %s to run %s", def.Name, def.BinPath), nil
	}

	// Compared against Command, never BinPath. BinPath is the ImagePath with
	// the SCM's own escaping still on it, while a Definition holds unescaped
	// values — so comparing the two would need a portable re-implementation of
	// EscapeArg, wrong in exactly the cases that matter.
	want := append([]string{def.BinPath}, def.Args...)

	if status.Command == nil {
		return blocked("%s is registered but its command line could not be read back (%q), "+
			"so this run cannot confirm it matches; re-run with --reinstall to replace it",
			def.Name, status.BinPath), nil
	}

	if sameCommand(status.Command, want) {
		return satisfied("%s is registered and already runs %s", def.Name, def.BinPath), nil
	}

	// Manager.Install returns ErrAlreadyInstalled and never reconfigures, so a
	// registration that differs is not something this run can quietly correct.
	if p.opts.Reinstall {
		return pending("re-register %s: it currently runs %v", def.Name, status.Command), nil
	}

	return blocked("%s is registered but runs %v, not %v; re-run with --reinstall to replace it",
		def.Name, status.Command, want), nil
}

func (s installStep) Apply(_ context.Context, p *Plan) error {
	if p.opts.Reinstall {
		if err := s.removeExisting(p); err != nil {
			return err
		}
	}

	result, err := Install(InstallOptions{
		Definition: p.opts.Definition,
		BinPath:    p.BinPath,
		ConfigPath: p.ConfigPath,
		Spec:       p.opts.Spec,
		Validate:   p.opts.Validate,
	}, p.deps)
	if err != nil {
		return err
	}

	p.Definition = result.Definition
	p.ConfigPath = result.ConfigPath
	p.BinPath = result.BinPath
	p.Secured = append(p.Secured, result.Secured...)

	return nil
}

// sameCommand reports whether the SCM's command line is the one this run would
// register.
//
// Case-INSENSITIVE, because every element compared is a Windows path or an
// ASCII flag name and Windows paths are case-insensitive: an operator who
// passed `--bin-dir "C:\Program Files\weave-adapters"` once and
// `"c:\program files\weave-adapters"` the next time named the same directory,
// and answering Blocked for that would demand --reinstall to fix a difference
// that is not one. The fold is safe only while that holds — an argument whose
// VALUE is case-sensitive would need this comparing element by element against
// what each one is.
func sameCommand(got, want []string) bool {
	return slices.EqualFunc(got, want, strings.EqualFold)
}

// removeExisting stops and deregisters a service that is already there, which
// is what --reinstall asked for. An absent service is not an error: the flag
// may have been passed on a host where the drift had already been cleared.
func (installStep) removeExisting(p *Plan) error {
	m, err := p.deps.NewManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(p.opts.Definition.Name)
	if err != nil {
		return fmt.Errorf("reading the state of %s: %w", p.opts.Definition.Name, err)
	}

	if !status.Installed {
		return nil
	}

	if err := m.Uninstall(p.opts.Definition.Name); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
		return fmt.Errorf("removing the existing registration of %s: %w", p.opts.Definition.Name, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

// startStep brings the service up, and refuses to bounce a running one unasked.
type startStep struct{}

func (startStep) Name() string { return "start" }

func (s startStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	m, err := p.deps.NewManager()
	if err != nil {
		return Verdict{}, err
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(p.opts.Definition.Name)
	if err != nil {
		return Verdict{}, fmt.Errorf("reading the state of %s: %w", p.opts.Definition.Name, err)
	}

	if !status.Installed || status.State != winsvc.StateRunning {
		return pending("start %s", p.opts.Definition.Name), nil
	}

	// Running, but something this run did will not take effect until it is
	// restarted. The commonest is a freshly minted token: buildAuth reads the
	// store once at startup, so rotation is restart-only by design.
	if p.restartWanted != "" {
		if p.opts.Restart {
			return pending("restart %s: %s", p.opts.Definition.Name, p.restartWanted), nil
		}

		return blocked("%s is running and needs a restart (%s); re-run with --restart",
			p.opts.Definition.Name, p.restartWanted), nil
	}

	return satisfied("%s is running", p.opts.Definition.Name), nil
}

func (s startStep) Apply(_ context.Context, p *Plan) error {
	m, err := p.deps.NewManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	name := p.opts.Definition.Name

	// Stop first when a restart is what was asked for. Stop is a desired
	// state rather than a transition — an already-stopped service is not an
	// error — so this is safe on a service that was never running.
	if p.restartWanted != "" {
		if err := m.Stop(name); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
	}

	if err := m.Start(name); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	return nil
}
