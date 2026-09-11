package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// managerFactory opens an SCM connection. Injected so the command's own logic
// — the flag parsing, the consent gate, the refusals, the output — is tested
// on any platform, rather than only on a host nobody can iterate on.
type managerFactory func() (winsvc.Manager, error)

// runService dispatches a service subcommand. newManager is injected; out
// receives all human-facing output.
func runService(args []string, out interface{ Write([]byte) (int, error) }, newManager managerFactory) error {
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
		return runServiceInstall(args[1:], p, newManager)
	case verbUninstall, verbStart, verbStop:
		return runServiceLifecycle(args[1:], p, newManager, args[0])
	case verbStatus:
		return runServiceStatus(args[1:], p, newManager)
	default:
		p.printf("%s", serviceUsage)

		return fmt.Errorf("service: unknown command %q", args[0])
	}
}

// runServiceInstall registers the service.
func runServiceInstall(args []string, p *printer, newManager managerFactory) error {
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

	m, err := newManager()
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

	p.printf("Installed %s.\n", serviceName)
	p.printf("  binary:     %s\n", binPath)
	p.printf("  config:     %s\n", absConfig)
	p.printf("  account:    LocalSystem\n")
	p.printf("  start type: automatic\n")
	p.printf("  recovery:   restart after 5s, 10s, then 60s\n")
	p.printf("\nStart it with: weave-adapter-dhcp-windows service start\n")

	return p.err
}

// runServiceLifecycle runs uninstall, start or stop, all of which take no
// flags beyond the shared ones and differ only in the verb.
func runServiceLifecycle(args []string, p *printer, newManager managerFactory, verb string) error {
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

	m, err := newManager()
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
func runServiceStatus(args []string, p *printer, newManager managerFactory) error {
	flags := flag.NewFlagSet("service status", flag.ContinueOnError)
	flags.SetOutput(p.w)

	if err := flags.Parse(args); err != nil {
		return skipHelp(err)
	}

	m, err := newManager()
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
