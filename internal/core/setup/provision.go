package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// provisionedDirMode is the mode the provisioning directory is created with.
// Windows ignores it — the DACL applied immediately afterwards is the real
// protection — and it keeps the directory owner-only on any other host.
const provisionedDirMode = 0o700

// configFileMode matches the token store's. Hygiene rather than the boundary,
// for the same reason: on Windows the ACL is what protects the file.
const configFileMode = 0o600

// ---------------------------------------------------------------------------
// directory
// ---------------------------------------------------------------------------

// directoryStep creates the provisioning directory and locks it down BEFORE
// anything is written into it.
//
// The ordering is the entire point. `os.WriteFile` then `Secure` leaves a
// window in which the config file — carrying identity.namespaceKey — inherits
// C:\ProgramData's default ACL, which grants read to local Users. Nothing in
// auth.Store.Save closes that window either: its os.Chmod is a no-op for ACL
// purposes on Windows. Locking the container first means every file created
// inside it inherits the locked-down entries and is never briefly readable.
type directoryStep struct{}

func (directoryStep) Name() string { return "directory" }

func (s directoryStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	dir := p.opts.Layout.Dir

	// Only when this run will actually put something there. An operator who
	// supplied their own config path elsewhere gets the per-file lockdown the
	// install step applies from THEIR resolved values; creating and locking an
	// empty directory they never asked for would be noise.
	if !s.owns(p) {
		return satisfied("the configuration is the operator's; its files are locked per-file at install"), nil
	}

	// Lstat, and before the Stat below, because Stat is what is being defended
	// against: C:\ProgramData admits any authenticated account to create a
	// subdirectory, so an unprivileged user who puts a junction at this path
	// before setup runs has every step below act on a directory of their own —
	// Stat follows it, the lockdown secures its target, and the config and the
	// token store are written through it.
	//
	// An error rather than a Blocked verdict: no flag should clear this, and
	// the operator has to look at what is there before anything else happens.
	if err := winsvc.CheckNotReparsePoint(dir); err != nil {
		return Verdict{}, err
	}

	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return pending("create %s and lock it to SYSTEM and Administrators", dir), nil
	}

	if err != nil {
		return Verdict{}, fmt.Errorf("reading %q: %w", dir, err)
	}

	if !info.IsDir() {
		return Verdict{}, fmt.Errorf("%q exists and is not a directory", dir)
	}

	// PolicyOwned, because this is a directory this package creates and locks
	// down itself: Apply sets the owner to Administrators along with the list,
	// so anything the lockdown has touched answers it. A directory that does
	// not is one somebody else created first — and this Check returning
	// satisfied is what would skip Apply and leave their ownership in place,
	// with the config and the token store written into it.
	//
	// Write grants and ownership only. A stray read entry on a directory is
	// not worth a refusal nobody can satisfy.
	if err := p.deps.CheckDir(dir, winsvc.PolicyOwned); err != nil {
		return pending("re-lock %s: %s", dir, err), nil
	}

	return satisfied("%s exists, is owned inside the policy, and grants write to nobody outside it", dir), nil
}

func (s directoryStep) Apply(_ context.Context, p *Plan) error {
	dir := p.opts.Layout.Dir

	if !s.owns(p) {
		return nil
	}

	if err := os.MkdirAll(dir, provisionedDirMode); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	results, err := p.deps.Secure([]winsvc.Securable{{
		Path: dir,
		Kind: winsvc.SecurableDirectory,
		// A directory, so the entries are inheritable and every file created
		// here afterwards — config, token store, log — is born locked down.
		Why: "the provisioning directory, whose entries every file in it inherits",
	}})
	if err != nil {
		return fmt.Errorf("locking down %q: %w", dir, err)
	}

	p.Secured = append(p.Secured, results...)

	return nil
}

// owns reports whether this run is provisioning into its own layout.
func (directoryStep) owns(p *Plan) bool {
	return p.ConfigPath == p.opts.Layout.ConfigPath
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// configStep writes the config file, and only when there is not one already.
//
// Never rewriting is a decision, not an optimisation. A rewrite goes through
// go-toml, which does not preserve comments or formatting, so an operator's
// annotated file would come back stripped — and a rewrite is how a value gets
// changed by accident, which for identity.namespaceKey is a fleet-wide re-key.
type configStep struct{}

func (configStep) Name() string { return "config" }

func (s configStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	_, err := os.Stat(p.ConfigPath)

	switch {
	case err == nil:
		return s.checkExisting(p)
	case errors.Is(err, fs.ErrNotExist):
		return s.checkProjected(p)
	default:
		return Verdict{}, fmt.Errorf("reading %q: %w", p.ConfigPath, err)
	}
}

// checkExisting validates the config already on disk and refuses any
// provisioned value that disagrees with it.
func (s configStep) checkExisting(p *Plan) (Verdict, error) {
	values, err := p.resolve(p.ConfigPath)
	if err != nil {
		return Verdict{}, err
	}

	if err := p.validate(values); err != nil {
		// An error rather than Blocked: Blocked means "will not, without a
		// flag", and no flag can make an unstartable configuration start. The
		// operator has to edit the file.
		return Verdict{}, fmt.Errorf(
			"the existing configuration at %q would not start as a service:\n%w", p.ConfigPath, err)
	}

	if err := s.refuseDisagreements(p, values); err != nil {
		return Verdict{}, err
	}

	p.Values = values
	p.ConfigExists = true

	return satisfied("%s exists and would start; it is left exactly as it is", p.ConfigPath), nil
}

// refuseDisagreements reports every provisioned value that differs from what
// the existing configuration already resolves to.
//
// Differs, not "was supplied". A re-run passing the same flags must finish the
// job rather than fail on them, which is the whole promise of re-running on a
// half-configured host — so an identical value is a no-op. A DIFFERENT value
// is refused rather than applied, because applying it would mean rewriting the
// file, and for identity.namespaceKey it would mean re-keying the fleet.
func (configStep) refuseDisagreements(p *Plan, values *config.Values) error {
	var errs []error

	for _, provisioned := range p.opts.Provisioned {
		got, err := config.ValueOf(values, provisioned.Key)
		if err != nil {
			return err
		}

		if p.opts.equivalent(provisioned.Key, provisioned.Value, got) {
			continue
		}

		// The value itself never appears in the message. One of these is
		// identity.namespaceKey, and an error that printed either half of the
		// comparison would put a backup-critical secret into scrollback and
		// into an MSI log.
		errs = append(errs, fmt.Errorf(
			"%s was supplied but differs from what %q already sets, and an existing configuration is "+
				"never rewritten: edit the file by hand, or point this run at a different one",
			provisioned.Key, p.ConfigPath))
	}

	return errors.Join(errs...)
}

// checkProjected renders the config this run would write, resolves it, and
// validates it — all in memory.
//
// In memory, and validated before anything is written, because the file
// carries a namespace key. A render-then-write-then-validate order leaves a
// failed run with a fresh key on disk that nobody asked for, and on the next
// run that key is indistinguishable from a provisioned one.
func (configStep) checkProjected(p *Plan) (Verdict, error) {
	data, err := config.Render(p.opts.Spec, p.opts.Provisioned)
	if err != nil {
		return Verdict{}, err
	}

	values, err := config.ResolveBytes(p.opts.Spec, p.ConfigPath, data)
	if err != nil {
		return Verdict{}, fmt.Errorf("the configuration this run would write does not parse: %w", err)
	}

	if err := p.validate(values); err != nil {
		return Verdict{}, fmt.Errorf(
			"the configuration this run would write would not start as a service:\n%w", err)
	}

	p.Values = values
	p.ConfigExists = false

	return pending("write %s", p.ConfigPath), nil
}

func (configStep) Apply(_ context.Context, p *Plan) error {
	data, err := config.Render(p.opts.Spec, p.opts.Provisioned)
	if err != nil {
		return err
	}

	// O_EXCL, not a stat-then-write. The check ran earlier and the host may
	// have changed since; exclusive creation is the only form in which "do not
	// overwrite an existing config" is not a race.
	f, err := os.OpenFile(p.ConfigPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%q appeared after this run started; an existing configuration is "+
				"never rewritten, so nothing was changed", p.ConfigPath)
		}

		return fmt.Errorf("creating %q: %w", p.ConfigPath, err)
	}

	if err := writeAndClose(f, data); err != nil {
		return err
	}

	// Re-resolved from the file rather than trusting the projection: what the
	// service will read is the bytes on disk, and anything that made the two
	// differ is worth finding now.
	values, err := p.resolve(p.ConfigPath)
	if err != nil {
		return err
	}

	p.Values = values

	return nil
}

// writeAndClose writes, flushes and closes, reporting whichever step failed.
// The close error matters: a deferred close would discard a failed flush.
func writeAndClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("writing %q: %w", f.Name(), err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("syncing %q: %w", f.Name(), err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", f.Name(), err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// token
// ---------------------------------------------------------------------------

// tokenStep mints the first bearer token.
type tokenStep struct{}

func (tokenStep) Name() string { return "token" }

func (s tokenStep) Check(_ context.Context, p *Plan) (Verdict, error) {
	if p.Values.Bool(config.KeyDisableAuth) {
		// Skipped, and said out loud. Minting into a store nothing reads and
		// then reporting success is worse than doing nothing: it leaves an
		// operator holding a token that will never be checked.
		return satisfied("%s is true, so nothing reads the token store; no token was minted",
			config.KeyDisableAuth), nil
	}

	store, err := auth.LoadOrEmpty(p.Values.String(config.KeyAuthTokensFile))
	if err != nil {
		return Verdict{}, err
	}

	// Usable, not len. A store whose tokens have all expired refuses every
	// request and stops the adapter starting, while plainly containing tokens
	// — counting entries would report "already done" for the harder of the two
	// failures to diagnose.
	if auth.NewVerifier(store.Tokens).Usable() > 0 {
		return satisfied("%s already holds a usable token", p.Values.String(config.KeyAuthTokensFile)), nil
	}

	// Add refuses a duplicate label, so a re-run against a store whose
	// same-label token has expired would fail at the mint with a duplicate
	// error that reads like a bug. Saying so here, with the way out, is the
	// difference between that and a two-minute fix.
	if entry, found := store.Find(p.opts.TokenLabel); found {
		return Verdict{}, fmt.Errorf(
			"the token labelled %q in %s has expired (%s) and the store refuses a duplicate label: "+
				"revoke it first with `token revoke --label %s`",
			entry.Label, p.Values.String(config.KeyAuthTokensFile),
			entry.ExpiresAt.Time().Format("2006-01-02"), entry.Label)
	}

	// Recorded HERE, not in Apply. Every Check runs before any Apply, so the
	// start step's Check — which is what turns this into a "needs --restart"
	// refusal — has already run by the time Apply would set it. Setting it
	// there instead mints the token, leaves the service running, and reports
	// success for a credential the service will not read until it restarts.
	p.wantsRestart("a token will be minted, and the store is read only at startup")

	return pending("mint a token labelled %q into %s",
		p.opts.TokenLabel, p.Values.String(config.KeyAuthTokensFile)), nil
}

func (s tokenStep) Apply(_ context.Context, p *Plan) error {
	path := p.Values.String(config.KeyAuthTokensFile)

	store, err := auth.LoadOrEmpty(path)
	if err != nil {
		return err
	}

	createdAt := p.opts.now().UTC()

	var expiry *auth.Expiry

	if p.opts.TokenExpiresInDays > 0 {
		expiry, err = auth.ExpiryInDays(createdAt, p.opts.TokenExpiresInDays)
		if err != nil {
			return fmt.Errorf("the requested token expiry of %d days: %w", p.opts.TokenExpiresInDays, err)
		}
	}

	// Mint adds; Save is separate and comes after, so a duplicate label fails
	// without the file being touched.
	token, err := store.Mint(p.opts.TokenLabel, createdAt, expiry)
	if err != nil {
		return err
	}

	if err := store.Save(path); err != nil {
		return err
	}

	p.Token = token

	// The store did not exist when the directory was locked, so on a layout
	// this run did not provision it has inherited whatever its directory
	// permits. Securing the file itself is also what the gate asserts:
	// inheriting the right entries is not the same as being protected.
	results, err := p.deps.Secure([]winsvc.Securable{{
		Path: path,
		Kind: winsvc.SecurableFile,
		Why:  "the bearer token store, where a write grants API access",
	}})
	if err != nil {
		return fmt.Errorf("locking down %q: %w", path, err)
	}

	p.Secured = append(p.Secured, results...)

	return nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// resolve loads a config file the way the service will: through the full spec,
// and without the process environment.
//
// Without it for the reason the installer already refuses to use it: this
// process is an elevated operator's shell, the service gets the machine
// environment under the SCM, and a value exported here would make every check
// pass and then be absent at every boot.
func (p *Plan) resolve(path string) (*config.Values, error) {
	values, err := config.LoadWithoutEnvironment(p.opts.Spec, []string{configFlag, path})
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}

	return values, nil
}

// validate runs every check that decides whether a configuration would start:
// the service-path rules, core's own validation, and the adapter's.
func (p *Plan) validate(values *config.Values) error {
	_, coreErr := config.Core(values)

	return errors.Join(config.CheckServicePaths(values), coreErr, p.opts.Validate(values))
}
