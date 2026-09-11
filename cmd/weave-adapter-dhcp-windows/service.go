package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// serviceUsage describes the subcommands.
const serviceUsage = `Usage: weave-adapter-dhcp-windows service <command> [flags]

Commands:
  install    register the adapter with the Windows Service Control Manager
  uninstall  stop and deregister it
  start      bring the installed service up
  stop       take it down, draining in-flight requests first
  status     show what the SCM knows about it
  secure     lock the config, token store and log directory to SYSTEM and
             Administrators only (install does this too)

Run every command from an ELEVATED prompt: the SCM refuses all of them
to a non-administrator.
`

// The lifecycle verbs, named so the dispatch and the operation cannot drift.
const (
	verbInstall   = "install"
	verbUninstall = "uninstall"
	verbStart     = "start"
	verbStop      = "stop"
	verbStatus    = "status"
	verbSecure    = "secure"
)

// consentFlag is the acknowledgement install requires. It follows the
// precedent scripts/grant-dhcp-access.ps1 sets: a privilege grant should not
// happen because somebody ran a command that sounded routine.
const consentFlag = "i-understand-this-runs-as-localsystem"

// localSystemNotice explains what the consent flag is consenting to.
const localSystemNotice = `This installs a service that runs as LocalSystem — the most privileged
account on this host.

That is not a preference. It was measured: the PowerShell DHCP cmdlets go
through WMI, which gates on Administrators. NETWORK SERVICE in DHCP Users,
NETWORK SERVICE in DHCP Administrators, and an ordinary user in DHCP Users
were all refused WIN32 5 on Windows Server 2022. A service account that
cannot read DHCP serves 503 from every endpoint. See docs/dhcp-backend.md.

The service also serves over plain HTTP: TLS is not implemented yet, so the
bearer token weave sends crosses the wire in clear. Running unattended from
boot makes it more likely somebody points real traffic at it.

Re-run with --` + consentFlag + ` to proceed.
`

// managerFactory opens an SCM connection.
type managerFactory func() (winsvc.Manager, error)

// secureFunc applies the file lockdown.
type secureFunc func([]winsvc.Securable) error

// serviceDeps are the platform operations this subcommand needs.
//
// Injected, both of them, so the command's own logic — the flag parsing, the
// consent gate, the refusals, which paths get secured, the output — is tested
// on any platform rather than only on a host nobody can iterate on. The
// decisions are the command's; only the syscalls are the platform's.
type serviceDeps struct {
	newManager managerFactory
	secure     secureFunc
}

// platformDeps are the real operations, used by main.
func platformDeps() serviceDeps {
	return serviceDeps{newManager: winsvc.NewManager, secure: winsvc.Secure}
}

// runService dispatches a service subcommand. newManager is injected; out
// receives all human-facing output.
func runService(args []string, out io.Writer, deps serviceDeps) error {
	p := &printer{w: out}

	if len(args) == 0 {
		p.printf("%s", serviceUsage)

		return errors.New("service: a command is required")
	}

	if isHelpVerb(args[0]) {
		p.printf("%s", serviceUsage)

		return p.err
	}

	switch args[0] {
	case verbInstall:
		return runServiceInstall(args[1:], p, deps)
	case verbUninstall, verbStart, verbStop:
		return runServiceLifecycle(args[1:], p, deps, args[0])
	case verbStatus:
		return runServiceStatus(args[1:], p, deps)
	case verbSecure:
		return runServiceSecure(args[1:], p, deps)
	default:
		p.printf("%s", serviceUsage)

		return fmt.Errorf("service: unknown command %q", args[0])
	}
}

// runServiceInstall registers the service.
func runServiceInstall(args []string, p *printer, deps serviceDeps) error {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	flags.SetOutput(p.w)

	configPath := flags.String("config", "", "path to the adapter's TOML config file (required)")
	consent := flags.Bool(consentFlag, false, "acknowledge that the service runs as LocalSystem")

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	if !*consent {
		p.printf("%s", localSystemNotice)

		return errors.New("service install: refused without consent")
	}

	// Required, not optional. identity.namespaceKey has no flag by design —
	// argv is world-readable and the key is backup-critical — and the SCM
	// gives a service no environment of its own that is not also
	// world-readable in the registry. The config file is the only channel
	// left, so a service installed without one cannot start at all.
	if *configPath == "" {
		return errors.New("service install: --config is required. " +
			"identity.namespaceKey cannot be passed as a flag (argv is readable by any local user) " +
			"and every environment channel under the SCM is readable from the registry, so the config " +
			"file is the only way the service can be given it")
	}

	// Resolved the way the server will, and WITHOUT the environment. This
	// shell is an elevated operator's; the service gets the machine
	// environment under the SCM. A value exported here would make every check
	// below pass and then be absent at every boot, which is a confident yes
	// for a service that cannot start — worse than not checking.
	//
	// Checking the whole resolved configuration rather than the flags this
	// command was handed is what catches a relative authTokensFile set inside
	// the TOML, which is where it is most likely to be.
	values, err := config.LoadWithoutEnvironment(
		append(config.CoreKeys(), dhcpwindows.Keys()...),
		[]string{"--config", *configPath},
	)
	if err != nil {
		return fmt.Errorf("service install: reading %q: %w", *configPath, err)
	}

	// Validated as fully as the server will, not merely parsed. The commonest
	// way for a service to fail every start is a configuration that would have
	// been rejected at the first console run — a missing identity.namespaceKey
	// above all — and the difference between catching it here and catching it
	// on the host is an error the operator reads now versus one they read in
	// Event Viewer after the SCM has retried three times.
	_, coreErr := config.Core(values)
	_, adapterErr := dhcpwindows.NewConfig(values)

	if err := errors.Join(config.CheckServicePaths(values), coreErr, adapterErr); err != nil {
		return fmt.Errorf("service install: this configuration would not start as a service:\n%w", err)
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service install: locating this executable: %w", err)
	}

	binPath, err = filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("service install: resolving %q: %w", binPath, err)
	}

	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return fmt.Errorf("service install: resolving %q: %w", *configPath, err)
	}

	m, err := deps.newManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	// Unquoted, deliberately: the SCM registration escapes the path and every
	// argument itself, and quoting here would produce a doubly-escaped
	// ImagePath and a service that cannot start.
	def := winsvc.Definition{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		BinPath:     binPath,
		Args:        []string{"--config", absConfig},
		DrainBudget: drainBudget,
	}

	if err := m.Install(def); err != nil {
		return fmt.Errorf("service install: %w", err)
	}

	// After registration, because a failure here leaves a registered service
	// whose paths are still wide -- and the error says exactly that, so an
	// operator knows the service exists and what is still owed.
	if err := secureConfiguredPaths(p, deps, absConfig, values); err != nil {
		return fmt.Errorf("service install: %s is registered, but securing its files failed: %w",
			serviceName, err)
	}

	p.printf("Installed %s.\n", serviceName)
	p.printf("  binary:     %s\n", binPath)
	p.printf("  config:     %s\n", absConfig)
	p.printf("  account:    LocalSystem\n")
	p.printf("  start type: automatic\n")
	p.printf("  recovery:   restart after %s\n", recoverySchedule())
	p.printf("  secured:    config, token store and log directory, to SYSTEM and Administrators\n")
	p.printf("\nStart it with: weave-adapter-dhcp-windows service start\n")

	return p.err
}

// runServiceLifecycle runs uninstall, start or stop, all of which take no
// flags beyond the shared ones and differ only in the verb.
func runServiceLifecycle(args []string, p *printer, deps serviceDeps, verb string) error {
	flags := flag.NewFlagSet("service "+verb, flag.ContinueOnError)
	flags.SetOutput(p.w)

	var confirm *bool
	if verb == verbUninstall {
		// The destructive half. It stops a running service, deletes the
		// registration and removes the Event Log source — the same precedent
		// install follows for a grant it cannot take back cheaply.
		confirm = flags.Bool("yes", false, "confirm removing the service and its Event Log source")
	}

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	if confirm != nil && !*confirm {
		p.printf("This stops %s, deletes its registration, and removes its Event Log source.\n", serviceName)
		p.printf("Re-run with --yes to proceed.\n")

		return errors.New("service uninstall: refused without --yes")
	}

	m, err := deps.newManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	var opErr error

	switch verb {
	case verbUninstall:
		opErr = m.Uninstall(serviceName)
	case verbStart:
		opErr = m.Start(serviceName)
	case verbStop:
		opErr = m.Stop(serviceName)
	}

	if opErr != nil {
		// Named rather than wrapped opaquely: "not installed" is the one an
		// operator most often hits, and it reads as a typo in the name
		// otherwise.
		if errors.Is(opErr, winsvc.ErrNotInstalled) {
			return fmt.Errorf("service %s: %s is not installed", verb, serviceName)
		}

		return fmt.Errorf("service %s: %w", verb, opErr)
	}

	p.printf("%s: %s\n", serviceName, pastTense(verb))

	return p.err
}

// recoverySchedule renders the restart delays for the install summary, from
// the slice that owns them rather than from a literal that would quietly stop
// matching the moment the defaults changed.
func recoverySchedule() string {
	parts := make([]string, 0, len(winsvc.DefaultRecovery))
	for _, d := range winsvc.DefaultRecovery {
		parts = append(parts, d.String())
	}

	return strings.Join(parts, ", then ")
}

// pastTense renders the verb for the confirmation line.
func pastTense(verb string) string {
	switch verb {
	case verbUninstall:
		return "uninstalled"
	case verbStart:
		return "started"
	case verbStop:
		return "stopped"
	default:
		return verb
	}
}

// runServiceStatus prints what the SCM knows.
func runServiceStatus(args []string, p *printer, deps serviceDeps) error {
	flags := flag.NewFlagSet("service status", flag.ContinueOnError)
	flags.SetOutput(p.w)

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	m, err := deps.newManager()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }()

	status, err := m.Status(serviceName)
	if err != nil {
		return fmt.Errorf("service status: %w", err)
	}

	if !status.Installed {
		p.printf("%s is not installed.\n", serviceName)

		return p.err
	}

	p.printf("%s\n", status.Name)
	p.printf("  state:        %s\n", status.State)
	p.printf("  start type:   %s\n", status.StartType)
	p.printf("  binary:       %s\n", status.BinPath)
	p.printf("  drain budget: %s\n", preshutdownDescription(status.PreshutdownTimeout))

	// Surfaced because its absence is otherwise invisible, and it is the
	// difference between a restart schedule that fires and one that does not.
	if status.RestartsOnCleanExit {
		p.printf("  recovery:     restarts on failure, including a clean non-zero exit\n")
	} else {
		p.printf("  recovery:     WILL NOT restart on a clean non-zero exit — " +
			"the restart schedule is inert; reinstall to fix\n")
	}

	return p.err
}

// preshutdownDescription renders the pre-shutdown budget, naming the default
// when the value is absent.
func preshutdownDescription(d time.Duration) string {
	if d <= 0 {
		return "not set (the SCM default applies)"
	}

	return d.String()
}

// runServiceSecure re-applies the lockdown without touching the registration.
//
// Exposed as its own verb so the console deployment keeps a one-command
// lockdown — it is what scripts/secure-token-store.ps1 used to be — and so an
// operator who moved the token store can re-secure it without reinstalling.
func runServiceSecure(args []string, p *printer, deps serviceDeps) error {
	flags := flag.NewFlagSet("service secure", flag.ContinueOnError)
	flags.SetOutput(p.w)

	configPath := flags.String("config", "", "path to the adapter's TOML config file (required)")

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	if *configPath == "" {
		return errors.New("service secure: --config is required; it names the file to secure and " +
			"tells this command where the token store and log are")
	}

	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return fmt.Errorf("service secure: resolving %q: %w", *configPath, err)
	}

	values, err := config.LoadWithoutEnvironment(
		append(config.CoreKeys(), dhcpwindows.Keys()...),
		[]string{"--config", absConfig},
	)
	if err != nil {
		return fmt.Errorf("service secure: reading %q: %w", absConfig, err)
	}

	if err := secureConfiguredPaths(p, deps, absConfig, values); err != nil {
		return fmt.Errorf("service secure: %w", err)
	}

	return p.err
}

// secureConfiguredPaths locks down everything the resolved configuration
// names, and reports each target so an operator can see what was covered.
func secureConfiguredPaths(p *printer, deps serviceDeps, absConfig string, values *config.Values) error {
	targets := winsvc.SecurablesFor(
		absConfig,
		values.String(config.KeyAuthTokensFile),
		values.String(config.KeyLogFile),
	)

	if err := deps.secure(targets); err != nil {
		return err
	}

	for _, t := range targets {
		p.printf("  secured %s (%s)\n", t.Path, t.Why)
	}

	return nil
}
