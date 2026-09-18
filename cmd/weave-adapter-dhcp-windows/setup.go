package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/setup"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// setupUsage describes the one command that provisions a host.
const setupUsage = `Usage: weave-adapter-dhcp-windows setup [flags]

Provisions this host in one command: writes the configuration, mints the first
bearer token, copies the binary, registers the service, starts it, and checks
that it came up.

Safe to re-run. Every step reports whether it is already done, and a re-run on
a half-configured host finishes the job rather than starting over. An existing
configuration file is NEVER rewritten.

  --dry-run   report what would be done and change nothing. This is also the
              command to run when the service will not start: it names the
              step that is in the way.

Run from an ELEVATED prompt: registering a service needs Administrator.
`

// Setup's own flags, named apart from the derived config-key flags so the two
// sets cannot collide.
const (
	flagDryRun          = "dry-run"
	flagGenerateKey     = "generate-namespace-key"
	flagNamespaceKeyFil = "namespace-key-file"
	flagTokenLabel      = "token-label"
	flagTokenDays       = "token-expires-in-days"
	flagTokenOut        = "token-out"
	flagNoCopy          = "no-copy"
	flagReinstall       = "reinstall"
	flagRestart         = "restart"
	flagDataDir         = "data-dir"
	flagBinDir          = "bin-dir"
	flagTolerate        = "tolerate-unhealthy-backend"
)

// generatedKeyBytes is the entropy behind a generated namespace key. It is
// rendered as hex, so the key is twice this many characters — comfortably past
// the 16-character floor the adapter enforces, and the same shape as the
// `openssl rand -hex 32` the documentation has always suggested.
const generatedKeyBytes = 32

// Default provisioning locations.
//
// %ProgramData% and %ProgramFiles% rather than anywhere of our choosing: the
// first is where machine-scoped application data belongs and the second is the
// one place whose ACL already refuses write to unprivileged accounts, which is
// exactly what install checks for before registering a LocalSystem service.
//
// Spelled with the environment variables expanded at run time rather than
// hardcoded, because a host may not have them on C:.
var (
	defaultDataDir = filepath.Join(programData(), "weave-adapters")
	defaultBinDir  = filepath.Join(programFiles(), "weave-adapters")
)

// programData and programFiles resolve the two Windows locations, falling back
// to the documented defaults when the variables are absent — which is every
// non-Windows host, where these paths exist only to be printed by --dry-run.
func programData() string {
	if dir := os.Getenv("ProgramData"); dir != "" {
		return dir
	}

	return `C:\ProgramData`
}

func programFiles() string {
	if dir := os.Getenv("ProgramFiles"); dir != "" {
		return dir
	}

	return `C:\Program Files`
}

// healthComponent is the component whose health decides whether a provisioning
// run was a complete success. Core reports every component; naming the one that
// matters is this binary's job, because only it knows which adapter this is.
const healthComponent = "dhcp-server"

// protectedPath is a route the bearer middleware guards, used to prove the
// service is reading the token store this run wrote. Health cannot serve for
// this: the auth middleware skips it by design.
const protectedPath = "/api/v1/scopes"

// Exit codes. They are the whole machine-readable contract while `--output
// json` is deferred: an MSI custom action sees a code and nothing else, and
// the service gate asserts on them.
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 2
	exitBlocked   = 3
	exitUnhealthy = 4
)

// setupResult is what runSetup reports to main, which owns os.Exit.
type setupResult struct {
	code int
}

// runSetup provisions this host. out receives all human-facing output.
func runSetup(ctx context.Context, args []string, out io.Writer, deps setup.Deps) (setupResult, error) {
	p := &printer{w: out}

	if len(args) > 0 && isHelpVerb(args[0]) {
		p.printf("%s", setupUsage)

		return setupResult{code: exitOK}, p.err
	}

	opts, tokenOut, tolerate, err := setupOptions(args, p, deps)
	if err != nil {
		return setupResult{code: exitUsage}, err
	}

	result, runErr := setup.Run(ctx, opts, deps)

	printSetupReport(p, result)

	// Before every early return below, including the failing one. A token is
	// minted once and persisted as a hash, so a run that minted one and then
	// failed at a later step would destroy the credential and occupy the label
	// on the way out — the exact failure `token gen` goes out of its way to
	// report rather than cause.
	tokenErr := reportToken(p, result, opts, tokenOut, deps.Secure)

	if runErr != nil {
		return setupResult{code: exitFailed}, runErr
	}

	if len(result.Blocked) > 0 {
		printBlocked(p, result)

		return setupResult{code: exitBlocked}, nil
	}

	if tokenErr != nil {
		return setupResult{code: exitFailed}, tokenErr
	}

	if result.ConfigWritten && !result.DryRun {
		p.printf("Provisioned into %s:\n", opts.ConfigFilePath())
		printProvisioned(p, opts.Provisioned)
		p.printf("\n")

		printGeneratedKey(p, opts.Provisioned, opts.ConfigFilePath())
	}

	printHealth(p, result)

	return setupResult{code: setupExitCode(result, tolerate)}, p.err
}

// setupExitCode maps a completed run to its code.
//
// The one that earns its own number is 4. "Installed, running, backend
// unhealthy" is what every developer machine and every CI runner answers,
// because neither has a DHCP backend — and automation has to be able to tell
// it from a step that failed. The tolerate flag maps it to 0 for the one
// caller that cannot: a WiX custom action with Return="check" fails the whole
// install on any non-zero code, and Return="ignore" would have discarded 1 and
// 3 as well, turning a genuinely failed install into a reported success.
func setupExitCode(result setup.Result, tolerate bool) int {
	if result.DryRun {
		// The same table: nothing blocked is 0, something blocked is 3, and a
		// check that errored already returned 1 above. Automation uses
		// --dry-run as a precheck, so these have to mean what they mean
		// everywhere else.
		return exitOK
	}

	if !result.Health.Checked || result.Health.ComponentHealthy {
		return exitOK
	}

	if tolerate {
		return exitOK
	}

	return exitUnhealthy
}

// setupOptions parses the command line into Options.
//
// Every configuration flag is DERIVED from the registered key set rather than
// spelled here, so this command has no vocabulary of its own: --log-file
// exists because logFile is a key, and identity.namespaceKey has no flag
// because it is registered NoFlag.
func setupOptions(args []string, p *printer, deps setup.Deps) (setup.Options, string, bool, error) {
	spec := adapterSpec()

	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(p.w)

	provisionedFrom, err := config.RegisterFlags(flags, spec)
	if err != nil {
		return setup.Options{}, "", false, err
	}

	var (
		configPath  = flags.String("config", "", "an existing config file to provision around, instead of writing one")
		dryRun      = flags.Bool(flagDryRun, false, "report what would be done and change nothing")
		generateKey = flags.Bool(flagGenerateKey, false,
			"generate identity.namespaceKey; refused unless the data directory is absent or empty and no service is registered")
		keyFile = flags.String(flagNamespaceKeyFil, "",
			"file holding identity.namespaceKey; exactly one trailing line ending is trimmed")
		tokenLabel = flags.String(flagTokenLabel, "", "label for the first bearer token (required)")
		tokenDays  = flags.Int(flagTokenDays, 0, "days until the first token expires (0 = never)")
		tokenOut   = flags.String(flagTokenOut, "",
			"write the minted token to this file instead of only printing it; the file is a live credential")
		noCopy    = flags.Bool(flagNoCopy, false, "register the binary where it is instead of copying it")
		reinstall = flags.Bool(flagReinstall, false, "replace a registration that differs from the desired one")
		restart   = flags.Bool(flagRestart, false, "stop and start a running service when a step needs it")
		dataDir   = flags.String(flagDataDir, defaultDataDir, "directory for the config, token store and log")
		binDir    = flags.String(flagBinDir, defaultBinDir, "directory the service binary is installed into")
		tolerate  = flags.Bool(flagTolerate, false,
			"exit 0 rather than 4 when the service is running but the backend is unhealthy")
		consent = flags.Bool(consentFlag, false, "acknowledge that the service runs as LocalSystem")
	)

	if err := flags.Parse(args); err != nil {
		return setup.Options{}, "", false, skipHelp(err)
	}

	// The consent gate covers mutation only. --dry-run changes nothing, and
	// requiring an acknowledgement to look at a host would make the flag a
	// reflex rather than a decision.
	if !*dryRun && !*consent {
		p.printf("%s", localSystemNotice)

		return setup.Options{}, "", false, errors.New("setup: refused without consent")
	}

	if *tokenLabel == "" {
		return setup.Options{}, "", false, fmt.Errorf("setup: --%s is required: the token store refuses a "+
			"duplicate label, so naming it is what makes a re-run against an expired token legible", flagTokenLabel)
	}

	layout := layoutFor(*dataDir, *binDir)

	provisioned := provisionedFrom()

	provisioned = withLayoutPaths(provisioned, layout, *configPath)

	provisioned, err = withNamespaceKey(provisioned, *keyFile, *generateKey, *configPath, layout, deps)
	if err != nil {
		return setup.Options{}, "", false, err
	}

	binPath, err := os.Executable()
	if err != nil {
		return setup.Options{}, "", false, fmt.Errorf("setup: locating this executable: %w", err)
	}

	return setup.Options{
		Spec: spec,
		Definition: winsvc.Definition{
			Name:        serviceName,
			DisplayName: serviceDisplayName,
			Description: serviceDescription,
			DrainBudget: drainBudget,
		},
		Validate:           validateAdapterConfig,
		HealthComponent:    healthComponent,
		ProtectedPath:      protectedPath,
		BinPath:            binPath,
		Layout:             layout,
		ConfigPath:         *configPath,
		Provisioned:        provisioned,
		TokenLabel:         *tokenLabel,
		TokenExpiresInDays: *tokenDays,
		DryRun:             *dryRun,
		NoCopy:             *noCopy,
		Reinstall:          *reinstall,
		Restart:            *restart,
	}, *tokenOut, *tolerate, nil
}

// layoutFor builds the provisioning layout from the two directory flags.
func layoutFor(dataDir, binDir string) setup.Layout {
	return setup.Layout{
		Dir:            dataDir,
		ConfigPath:     filepath.Join(dataDir, "config.toml"),
		TokenStorePath: filepath.Join(dataDir, config.DefaultAuthTokensFile),
		LogPath:        filepath.Join(dataDir, "adapter.log"),
		BinDir:         binDir,
	}
}

// withLayoutPaths fills in the two path keys the layout decides, unless the
// operator set them explicitly or brought a configuration of their own.
//
// Skipped entirely for an existing --config, and that is not an optimisation.
// An existing configuration is never rewritten, and a provisioned value that
// disagrees with it is refused — so injecting a default the operator never
// typed turns "their logFile is somewhere else" into a refusal of a flag they
// did not pass.
//
// logFile is filled in at all, rather than left to the console default,
// because once the process runs as a service the SCM discards stdout: a
// service without one runs correctly and logs nowhere, which somebody
// discovers during an incident.
//
// A slice rather than a map, so two runs given the same values produce the
// same file: Render writes keys in the order it is handed them, and map
// iteration order is not one.
func withLayoutPaths(provisioned []config.Provisioned, layout setup.Layout, configPath string) []config.Provisioned {
	if configPath != "" {
		return provisioned
	}

	defaults := []config.Provisioned{
		{Key: config.KeyAuthTokensFile, Value: layout.TokenStorePath},
		{Key: config.KeyLogFile, Value: layout.LogPath},
	}

	for _, value := range defaults {
		if hasKey(provisioned, value.Key) {
			continue
		}

		provisioned = append(provisioned, value)
	}

	return provisioned
}

// withNamespaceKey resolves identity.namespaceKey, which is never a flag value
// and is never printed.
//
// Two channels, not three. A flag would put a backup-critical secret into
// argv, where any local user can read it from a process listing; stdin was
// dropped because bufio.Scanner echoes on a terminal and hidden input needs a
// dependency this module does not carry. Neither remaining channel echoes.
func withNamespaceKey(
	provisioned []config.Provisioned,
	keyFile string,
	generate bool,
	configPath string,
	layout setup.Layout,
	deps setup.Deps,
) ([]config.Provisioned, error) {
	switch {
	case keyFile != "" && generate:
		return nil, fmt.Errorf("setup: --%s and --%s both name where the key comes from; pass one",
			flagNamespaceKeyFil, flagGenerateKey)

	case keyFile != "":
		key, err := readNamespaceKey(keyFile)
		if err != nil {
			return nil, err
		}

		return append(provisioned, config.Provisioned{
			Key: dhcpwindows.KeyNamespaceKey, Value: key, Secret: true,
		}), nil

	case generate:
		if err := guardRekey(configPath, layout, deps); err != nil {
			return nil, err
		}

		key, err := generateNamespaceKey()
		if err != nil {
			return nil, err
		}

		return append(provisioned, config.Provisioned{
			Key: dhcpwindows.KeyNamespaceKey, Value: key, Secret: true, FreshlyGenerated: true,
		}), nil

	default:
		// Neither. That is legal: the host may already have a config carrying
		// the key, which setup validates and leaves alone.
		return provisioned, nil
	}
}

// guardRekey refuses to generate a key on a host that has had one.
//
// The asymmetry decides the rule. A wrong generate re-derives every wadaptID
// at once: weave sees every scope as gone, proposes to recreate each, and
// Windows refuses on the one-scope-per-subnet rule — fleet-wide sync paralysis
// that no rollback fixes, because the old key is gone. A false refusal is one
// message naming what was found.
//
// So the bar is high on purpose: the data directory must be absent or EMPTY,
// and no service may be registered. A leftover token store or log proves a key
// existed on this host even when the config has been deleted, and a
// registration proves it even when the whole directory has.
//
// It gates only --generate-namespace-key. Staging a key file in that directory
// and passing --namespace-key-file is unaffected, which is the deliberate
// escape hatch.
func guardRekey(configPath string, layout setup.Layout, deps setup.Deps) error {
	if configPath != "" {
		return fmt.Errorf("setup: --%s was passed together with --config: an existing configuration "+
			"already carries identity.namespaceKey, and generating a second one would re-key this host",
			flagGenerateKey)
	}

	entries, err := os.ReadDir(layout.Dir)

	switch {
	case err == nil && len(entries) > 0:
		return fmt.Errorf("setup: --%s was passed but %s is not empty (%s): something on this host has "+
			"held a namespace key, and generating a fresh one re-derives every ID weave has seen. "+
			"Pass --%s with the key this host was provisioned with, or empty the directory deliberately",
			flagGenerateKey, layout.Dir, describeEntries(entries), flagNamespaceKeyFil)

	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("setup: reading %q: %w", layout.Dir, err)
	}

	return guardRegistered(deps)
}

// guardRegistered refuses to generate a key when a service is already
// registered, whatever the data directory looks like.
func guardRegistered(deps setup.Deps) error {
	m, err := deps.NewManager()

	switch {
	case errors.Is(err, winsvc.ErrUnsupported):
		// Not Windows, so there is categorically no Windows service to be
		// re-keying. The directory check above has already run.
		return nil

	case err != nil:
		// Any other failure means this run could NOT look, and "could not
		// look" is not "nothing is there" — an SCM that refused an
		// unelevated process says nothing about what is registered. The
		// guard's asymmetry decides it: a wrong generate is fleet-wide sync
		// paralysis no rollback undoes, and a false refusal is one message.
		return fmt.Errorf("setup: --%s was passed but the service control manager could not be "+
			"consulted, so this run cannot rule out an existing registration: %w", flagGenerateKey, err)
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(serviceName)
	if err != nil {
		return fmt.Errorf("setup: reading the state of %s: %w", serviceName, err)
	}

	if status.Installed {
		return fmt.Errorf("setup: --%s was passed but %s is already registered: it was provisioned with "+
			"a namespace key, and generating a fresh one re-derives every ID weave has seen. Pass --%s "+
			"with the key this host was provisioned with",
			flagGenerateKey, serviceName, flagNamespaceKeyFil)
	}

	return nil
}

// describeEntries names what was found, so a refusal is actionable rather than
// merely correct.
func describeEntries(entries []fs.DirEntry) string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	if len(names) > 3 {
		names = append(names[:3], "…")
	}

	return strings.Join(names, ", ")
}

// readNamespaceKey reads a key from a file.
//
// Exactly one trailing line ending is trimmed, and nothing else. `openssl rand
// -hex 32 > key` leaves a newline, and an untrimmed one becomes part of the
// key — so the fingerprint never matches what the operator computes by hand
// and every derived ID is silently wrong. Trimming more than one would be its
// own trap: whitespace inside a deliberately chosen key is the operator's.
func readNamespaceKey(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path, by design
	if err != nil {
		return "", fmt.Errorf("setup: reading the namespace key from %q: %w", path, err)
	}

	key := strings.TrimSuffix(string(data), "\n")
	key = strings.TrimSuffix(key, "\r")

	if key == "" {
		return "", fmt.Errorf("setup: %q is empty", path)
	}

	return key, nil
}

// generateNamespaceKey returns a fresh key, hex-encoded.
func generateNamespaceKey() (string, error) {
	buf := make([]byte, generatedKeyBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("setup: generating a namespace key: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// hasKey reports whether a key was already provisioned.
func hasKey(provisioned []config.Provisioned, key string) bool {
	for _, p := range provisioned {
		if p.Key == key {
			return true
		}
	}

	return false
}

// httpGet is the real GET the verification step uses.
func httpGet(ctx context.Context, url, bearer string) (setup.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return setup.Response{}, fmt.Errorf("building a request for %s: %w", url, err)
	}

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	// Its own client rather than http.DefaultClient: a provisioning run polls
	// a service that is still coming up, and an unbounded default would hang
	// a privileged command on a socket that never answers.
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return setup.Response{}, fmt.Errorf("requesting %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return setup.Response{}, fmt.Errorf("reading the response from %s: %w", url, err)
	}

	return setup.Response{Status: resp.StatusCode, Body: body}, nil
}
