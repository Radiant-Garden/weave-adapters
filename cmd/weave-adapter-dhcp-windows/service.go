package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/setup"
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

// platformDeps are the real operations, used by main.
func platformDeps() setup.Deps {
	return setup.Deps{
		NewManager: winsvc.NewManager,
		Secure:     winsvc.Secure,
		CheckDir:   checkDirectoryGrants,
	}
}

// checkDirectoryGrants reports whether dir grants write to anyone outside the
// lockdown policy.
func checkDirectoryGrants(dir string) error {
	grants, err := winsvc.ReadGrants(dir)
	if err != nil {
		// Unreadable is not the same as insecure, and refusing to install over
		// a descriptor this process could not read would block an operator for
		// a permissions quirk rather than a risk. Reported at debug rather
		// than swallowed silently, so it is findable if it ever matters.
		slog.Debug("could not read the access list of the binary's directory", "dir", dir, "error", err)

		return nil
	}

	return winsvc.CheckGrants(dir, grants)
}

// runService dispatches a service subcommand. newManager is injected; out
// receives all human-facing output.
func runService(args []string, out io.Writer, deps setup.Deps) error {
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
func runServiceInstall(args []string, p *printer, deps setup.Deps) error {
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

	// The file this process was launched from. setup takes it as a value
	// rather than asking for it, because the caller is not always registering
	// what it is running from -- M4b's `setup` may copy the binary into
	// %ProgramFiles% first and must then register the destination.
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("service install: locating this executable: %w", err)
	}

	// Everything from here is setup.Install: resolve without the environment,
	// validate as fully as the server would, refuse a binary directory others
	// can write, register, then secure. The adapter-bound half of the
	// validation arrives as a value, because core must not import the adapter.
	result, err := setup.Install(setup.InstallOptions{
		Definition: winsvc.Definition{
			Name:        serviceName,
			DisplayName: serviceDisplayName,
			Description: serviceDescription,
			DrainBudget: drainBudget,
		},
		BinPath:    binPath,
		ConfigPath: *configPath,
		Spec:       adapterSpec(),
		Validate:   validateAdapterConfig,
	}, deps)
	if err != nil {
		return fmt.Errorf("service install: %w", err)
	}

	printSecured(p, result.Secured)

	p.printf("Installed %s.\n", serviceName)
	p.printf("  binary:     %s\n", result.BinPath)
	p.printf("  config:     %s\n", result.ConfigPath)
	p.printf("  account:    LocalSystem\n")
	p.printf("  start type: automatic\n")
	p.printf("  recovery:   restart after %s\n", recoverySchedule())
	p.printf("\nStart it with: weave-adapter-dhcp-windows service start\n")

	return p.err
}

// runServiceLifecycle runs uninstall, start or stop, all of which take no
// flags beyond the shared ones and differ only in the verb.
func runServiceLifecycle(args []string, p *printer, deps setup.Deps, verb string) error {
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

	m, err := deps.NewManager()
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
func runServiceStatus(args []string, p *printer, deps setup.Deps) error {
	flags := flag.NewFlagSet("service status", flag.ContinueOnError)
	flags.SetOutput(p.w)

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	m, err := deps.NewManager()
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
	p.printf("  pre-shutdown: %s\n", preshutdownDescription(status.PreshutdownTimeout))

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
func runServiceSecure(args []string, p *printer, deps setup.Deps) error {
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

	values, err := config.LoadWithoutEnvironment(adapterSpec(), []string{"--config", absConfig})
	if err != nil {
		return fmt.Errorf("service secure: reading %q: %w", absConfig, err)
	}

	// The same validation install runs, and not optional here. A relative
	// logFile makes the log directory ".", and securing that would apply a
	// protected, inheriting SYSTEM-and-Administrators list to whatever
	// directory the operator happened to run from.
	if err := config.CheckServicePaths(values); err != nil {
		return fmt.Errorf("service secure: this configuration names paths a service cannot use:\n%w", err)
	}

	results, err := setup.SecurePaths(deps, absConfig, values)
	if err != nil {
		return fmt.Errorf("service secure: %w", err)
	}

	printSecured(p, results)

	return p.err
}

// printSecured reports each lockdown target so an operator can see what was
// covered. setup.SecurePaths decides what gets covered; the wording is this
// command's.
func printSecured(p *printer, results []winsvc.SecureResult) {
	for _, r := range results {
		if r.Applied {
			p.printf("  secured %s (%s)\n", r.Target.Path, r.Target.Why)

			continue
		}

		// Said plainly, because the alternative is an operator who mints a
		// token after install and believes the store is locked when it
		// inherited the directory's defaults.
		p.printf("  NOT YET %s (%s) — it does not exist. Run `service secure` again after `token gen`.\n",
			r.Target.Path, r.Target.Why)
	}
}
