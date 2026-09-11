// Command weave-adapter-dhcp-windows is the REST adapter that will expose
// Windows Server DHCP behind the uniform weave-adapters HTTP API.
//
// It serves GET /api/v1/health — whose dhcp-server component runs a real scope
// query against the backend — GET /openapi.yaml, which returns the contract the
// resource endpoints satisfy, and GET /api/v1/scopes, the first real resource:
// authenticated, ETagged and cursor-paginated.
//
// Logging goes through the cataloged events system (see internal/core/events);
// the HTTP server emits its own lifecycle events, so main only marks startup.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Aliased: the served spec and the adapter implementation are both package
	// dhcpwindows, one describing the contract and one honouring it.
	apispec "github.com/radiantgarden/weave-adapters/api/dhcp-windows"
	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/etag"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
	"github.com/radiantgarden/weave-adapters/internal/core/middleware"
	"github.com/radiantgarden/weave-adapters/internal/core/observability"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// version is the adapter version, overridable via -ldflags at build time.
var version = "0.0.0-dev"

// serviceName is the name the adapter registers under with the Windows Service
// Control Manager. It is also the Event Log source name, so the entries an
// operator filters for carry it.
//
// Fixed rather than configurable: an operator who can rename the service can
// register two of them against one host, and the second one's wadaptIDs would
// collide with the first's while looking like a different service.
const serviceName = "wadapt-dhcp-windows"

// What services.msc shows. The display name leads with weave because that is
// what an operator scanning the list is looking for, and the description says
// what breaks if the service is stopped rather than restating the name.
const (
	serviceDisplayName = "weave DHCP adapter (Windows)"
	serviceDescription = "Serves this host's Windows DHCP scopes to weave over the uniform adapter HTTP API. " +
		"While this is stopped, weave cannot read or change DHCP on this server."
)

// Run modes, as reported by SYS-001's runMode field.
const (
	runModeConsole = "console"
	runModeService = "service"
)

// drainBudget is how long in-flight requests get after shutdown begins.
//
// It lives here, in the binary, because two components need the same number and
// they are in packages that must not know about each other: the HTTP server
// drains within it, and the Windows service runner reports a WaitHint to the
// SCM derived from it. Promising the SCM less time than the server takes gets a
// legitimate drain killed; the two drifting apart is only avoidable if one
// place owns the value.
const drainBudget = httpserver.DefaultShutdownGrace

// main owns the three things run must not: signal wiring, the CLI-vs-server
// split, and the process exit code. It is the only place that calls os.Exit, so
// every startup path stays testable through run.
func main() {
	args := os.Args[1:]

	var err error

	switch {
	case isServiceCommand(args):
		err = runService(args[1:], os.Stdout, platformDeps())
		if err != nil {
			// A CLI mistake is not a startup failure, the same reasoning the
			// token arm applies: an operator who forgot --config gets a plain
			// message, never a structured SYS-005 claiming the adapter failed
			// to start.
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	case isTokenCommand(args):
		err = runToken(args[1:], os.Stdout, time.Now)
		if err != nil {
			// A CLI mistake (bad flag, duplicate label) is not a startup
			// failure: it gets a plain message on stderr, never a SYS-005
			// event. Emitting one would hand an operator who typo'd a flag a
			// structured log line claiming the adapter failed to start.
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	default:
		err = runServer(args)
	}

	if err != nil {
		os.Exit(1)
	}
}

// isServiceCommand reports whether args invoke service management rather than
// a server run.
func isServiceCommand(args []string) bool {
	return len(args) > 0 && args[0] == "service"
}

// isTokenCommand reports whether args invoke token management rather than a
// server run.
func isTokenCommand(args []string) bool {
	return len(args) > 0 && args[0] == "token"
}

// launch is what differs between the two ways the adapter starts. It is a
// struct rather than three parameters because every one of them is optional
// from run's point of view and all three move together.
type launch struct {
	// mode is reported as SYS-001's runMode.
	mode string
	// ready is invoked once the listener is bound. The SCM arm reports Running
	// from it; the console arm has nobody to tell.
	ready func()
	// sinks are extra log handlers — the Event Log arm, under the SCM.
	sinks []slog.Handler
}

// runServer dispatches to the arm that matches how this process was started.
//
// Detected rather than flagged: an operator cannot then start the service
// wrongly, and the console path stays byte-identical for development and for
// the smoke and e2e gates.
func runServer(args []string) error {
	isService, err := winsvc.IsService()
	if err != nil {
		// Not guessed at. Running the console arm under the SCM produces a
		// process that never reports Running and is killed for it; running the
		// SCM arm from a console fails to reach a dispatcher. Neither is better
		// than saying so.
		return reportOutcome(fmt.Errorf("determining whether this is a service: %w", err))
	}

	if isService {
		return runUnderSCM(args)
	}

	return runConsole(args)
}

// runUnderSCM serves under the Service Control Manager.
func runUnderSCM(args []string) error {
	// Installed before anything else, and this ordering is the point: the SCM
	// discards stdout, and a config error happens before any log file could
	// have been opened. Without the Event Log arm in place first, the one
	// failure an operator most needs to see — the service that will not start —
	// is reported into nothing.
	eventLog, closeEventLog, err := winsvc.NewEventLogHandler(serviceName)
	if err != nil {
		// Genuinely nowhere to report this. The SCM sees a non-zero exit.
		return err
	}

	defer func() { _ = closeEventLog.Close() }()

	slog.SetDefault(slog.New(eventLog))

	// Written by the closure and read after winsvc.Run returns, which is after
	// serve returned — so there is no concurrent access, and no need for the
	// mutex that shape usually wants.
	var logClose io.Closer

	err = winsvc.Run(serviceName, drainBudget, func(ctx context.Context, ready func()) error {
		var runErr error

		logClose, runErr = runWith(ctx, args, launch{
			mode:  runModeService,
			ready: ready,
			sinks: []slog.Handler{eventLog},
		})

		return runErr
	})

	outcome := reportOutcome(err)

	if logClose != nil {
		_ = logClose.Close()
	}

	return outcome
}

// runConsole runs the adapter until a signal arrives, reporting a startup
// failure as SYS-005.
func runConsole(args []string) error {
	// On Windows Server 2022 a console exe receives os.Interrupt (Ctrl+C,
	// CTRL_CLOSE); SIGTERM is a no-op there but keeps Unix dev parity.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	logClose, err := run(ctx, args)

	// Undeferred so the signal handler is released before the SYS-005 emit
	// below, rather than staying installed across shutdown reporting.
	stop()

	outcome := reportOutcome(err)

	// Closed AFTER the outcome is reported, never in run's own defer.
	//
	// This ordering is the whole feature. run used to close the log file on its
	// way out, so by the time SYS-005 was emitted the default handler pointed at
	// a closed file, the write failed, and slog discards handler errors --
	// leaving a failed startup with no record anywhere, not even on the console.
	// That is precisely the case logFile exists to cover.
	if logClose != nil {
		_ = logClose.Close()
	}

	return outcome
}

// reportOutcome emits SYS-005 for a startup failure and returns the error the
// process should exit on.
//
// Separated from runServer because the log sink has to outlive it and because
// the SCM arm needs the same reporting: a service that fails to start must say
// so through the same event, from the one place that decides what counts as a
// startup failure.
func reportOutcome(err error) error {
	// --help is a successful invocation, not a startup failure. The FlagSet has
	// already written the usage text; without this the operator who asked for it
	// gets a SYS-005 "startup failed: flag: help requested" and exit 1, while
	// `token gen --help` exits 0 out of the same binary.
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	if err != nil && !errors.Is(err, httpserver.ErrShutdownIncomplete) {
		// Startup failures are operational outcomes, not stray slog calls.
		// observability.Setup may not have run yet; the events system writes to
		// slog.Default either way.
		//
		// A drain that overran its grace period is excluded: it is not a startup
		// failure, it may follow days of healthy serving, and httpserver owns
		// SYS-007 for it already. Re-reporting it here would put "startup
		// failed" in the log for a process that started fine.
		events.Emit(context.Background(), catalog.SYS005, "error", err.Error())
	}

	return err
}

// run wires the adapter together and serves until ctx is cancelled. It returns
// errors rather than exiting so the whole startup path can be driven from tests.
//
// The returned Closer owns the log file, and it is the caller's to close --
// after the outcome has been reported, not before. It is nil when no file was
// opened, which includes every failure earlier than the logging setup.
func run(ctx context.Context, args []string) (io.Closer, error) {
	return runWith(ctx, args, launch{mode: runModeConsole, ready: func() {}})
}

// runWith is run with the launch-specific pieces supplied.
func runWith(ctx context.Context, args []string, l launch) (io.Closer, error) {
	// Taken before any work so uptime measures the process, not the server.
	started := time.Now()

	// The spec is composed here, at the one place that knows which adapter this
	// binary is. Core owns the precedence machinery, never the key set.
	values, err := config.Load(append(config.CoreKeys(), dhcpwindows.Keys()...), args)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Both halves are built from one resolved set, and their errors are joined
	// so an operator sees every problem in one run rather than one per restart.
	cfg, coreErr := config.Core(values)
	adapterCfg, adapterErr := dhcpwindows.NewConfig(values)

	if err := errors.Join(coreErr, adapterErr); err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// Belt and braces to the installer's own check, and not redundant with it.
	// The installer resolves without the environment, deliberately, so a
	// relative path arriving from a machine-wide variable is invisible to it —
	// this is the only altitude that can see one. It costs a map walk at
	// startup and turns a file-not-found an operator reads as a bug into a
	// message that names the key and the working directory.
	if l.mode == runModeService {
		if err := config.CheckServicePaths(values); err != nil {
			return nil, fmt.Errorf("this configuration cannot work as a service:\n%w", err)
		}

		// Refused, never repaired. A service that rewrote its own ACLs at boot
		// would quietly undo a deliberate change an operator made, and would
		// need write access to its own security descriptor to do it — which is
		// the thing being protected. `service secure` fixes it, from a prompt
		// that already has the rights.
		if err := checkFileSecurity(cfg); err != nil {
			return nil, err
		}
	}

	// Before the first Emit, and its failure is returned rather than logged:
	// a log sink that could not be opened is the one error that cannot report
	// itself through the log.
	_, logClose, err := observability.Setup(cfg.LogSeverity, cfg.LogFile, l.sinks...)
	if err != nil {
		return nil, fmt.Errorf("setting up logging: %w", err)
	}

	// Importing the catalog package registers the core events from init(), which
	// panics on a contract violation — so by this line the catalog is known good.
	events.Emit(ctx, catalog.SYS001, "version", version, "runMode", l.mode)

	// Emitted here rather than inside the adapter because startup events are
	// owned by the binary — the same split as SYS-001, which the core catalog
	// registers and this package emits.
	//
	// The read path is stateless, so nothing persists a previous identity to
	// compare against. This one line is what makes an accidental re-key
	// diagnosable at the moment it happens, instead of hours later from a wall
	// of sync failures.
	events.Emit(ctx, adapterevents.DHCP001,
		"serverName", adapterCfg.ServerName,
		"namespaceKeyFingerprint", dhcpwindows.NamespaceKeyFingerprint(adapterCfg.NamespaceKey),
	)

	authMiddleware, err := buildAuth(ctx, cfg)
	if err != nil {
		return logClose, err
	}

	// The probe issues a real scope query, so a green dhcp-server component
	// means the DhcpServer module is present, the service account can read, and
	// the server answers — not merely that a Windows service is running.
	backend := dhcpwindows.NewClient(adapterCfg)
	probe := dhcpwindows.NewProbe(backend, adapterCfg)

	// The spec and the routes are supplied here for the same reason the config
	// spec is: this is the one place that knows which adapter this binary is.
	// httpserver is core and must never import an adapter, so the document
	// arrives as bytes and the routes as values.
	//
	// etag.Conditional wraps the handler rather than the chain. It buffers the
	// response to hash it, which is right for a JSON collection and wrong for a
	// stream, so the choice belongs to whoever writes the handler — and a list
	// weave polls is exactly the case a 304 saves the most work on.
	// One handler serves both methods on the collection, so it is wrapped once
	// and mounted twice. etag.Conditional passes non-GET straight through
	// untouched, so wrapping it does not put an ETag on the 201.
	scopes := etag.Conditional(dhcpwindows.NewScopesHandler(backend, adapterCfg, cfg.MaxRequestBodyBytes))
	scope := etag.Conditional(dhcpwindows.NewScopeHandler(backend, cfg.MaxRequestBodyBytes))

	addr := fmt.Sprintf(":%d", cfg.Port)

	// The write bound must clear the slowest honest handler, which is one backed
	// by a full-length backend call plus the runner's kill grace, or a legitimate
	// slow response would be truncated into a torn body rather than the classified
	// 502/504 the backend errors promise. The margin absorbs JSON encoding and
	// scheduling. Only the binary knows the backend timeout, which is why the
	// server takes this as a value.
	writeTimeout := adapterCfg.CommandTimeout + dhcpwindows.RunnerKillGrace + 5*time.Second

	srv := httpserver.New(addr, health.NewHandler(version, started, probe),
		httpserver.WithInnerMiddleware(authMiddleware...),
		httpserver.WithOpenAPISpec(apispec.Spec()),
		httpserver.WithWriteTimeout(writeTimeout),
		httpserver.WithShutdownGrace(drainBudget),
		httpserver.WithReadyFunc(l.ready),
		httpserver.WithRoutes(
			httpserver.Route{
				Pattern: "GET " + dhcpwindows.ScopesPath,
				Handler: scopes,
			},
			httpserver.Route{
				Pattern: "POST " + dhcpwindows.ScopesPath,
				Handler: scopes,
			},
			// Mounted with POST rather than after it, because a create's
			// Location header points here and a 201 pointing at a 404 tells a
			// client the create did not happen. DELETE and PATCH share the same
			// wrapped handler: etag.Conditional passes non-GET straight through,
			// so a 204 or a PATCH 200 is served without an ETag while the GET
			// still gets one.
			httpserver.Route{
				Pattern: "GET " + dhcpwindows.ScopeItemPath,
				Handler: scope,
			},
			httpserver.Route{
				Pattern: "DELETE " + dhcpwindows.ScopeItemPath,
				Handler: scope,
			},
			httpserver.Route{
				Pattern: "PATCH " + dhcpwindows.ScopeItemPath,
				Handler: scope,
			},
		),
	)

	return logClose, srv.Run(ctx)
}

// buildAuth loads the token store and returns the authentication middleware, or
// no middleware at all when auth is disabled.
//
// Tokens are read once, here: rotation is restart-only by design, so there is
// no watcher and no reload path.
func buildAuth(ctx context.Context, cfg *config.Config) ([]middleware.Middleware, error) {
	if cfg.DisableAuth {
		// Loud, and cataloged rather than a bare log line: a server running
		// wide open is exactly the state an operator must be able to find
		// later.
		events.Emit(ctx, catalog.SYS006)

		return nil, nil
	}

	store, err := auth.Load(cfg.AuthTokensFile)
	if err != nil {
		return nil, fmt.Errorf("loading tokens from %q (run `token gen --label <name>` to create one): %w",
			cfg.AuthTokensFile, err)
	}

	verifier := auth.NewVerifier(store.Tokens)
	if verifier.Usable() == 0 {
		// An allow-list nothing can match would reject every request, which
		// looks like a bug to whoever is on call. Fail at startup, where the
		// message can say what to do.
		//
		// Usable, not Len: a store whose every token has expired counts as
		// non-empty but accepts nothing, and it is the worse of the two to be
		// paged for — the file visibly contains tokens, so the 401s read as an
		// auth bug rather than as expiry.
		if verifier.Len() > 0 {
			return nil, fmt.Errorf(
				"all %d tokens in %q have expired: run `token gen --label <name>` to mint a replacement",
				verifier.Len(), cfg.AuthTokensFile,
			)
		}

		return nil, fmt.Errorf("no tokens configured in %q: run `token gen --label <name>` or set disableAuth",
			cfg.AuthTokensFile)
	}

	return []middleware.Middleware{auth.Bearer(verifier, httpserver.Unauthenticated)}, nil
}

// checkFileSecurity refuses to start when a file the service depends on
// grants write to anyone outside SYSTEM and Administrators.
//
// The token store is the sharpest case: the adapter reads it once at startup
// and trusts every hash in it, so anyone who can append one has a bearer
// token the API accepts. The config file is next — it carries
// identity.namespaceKey, and a write re-keys every wadaptID on the host.
func checkFileSecurity(cfg *config.Config) error {
	var errs []error

	for _, path := range []string{cfg.AuthTokensFile, cfg.LogFile} {
		if path == "" {
			continue
		}

		grants, err := winsvc.ReadGrants(path)
		if err != nil {
			// Unreadable is not the same as insecure, and refusing to start
			// over a descriptor this process could not read would take the
			// adapter down for a permissions quirk rather than a risk.
			slog.Debug("could not read the access list", "path", path, "error", err)

			continue
		}

		errs = append(errs, winsvc.CheckGrants(path, grants))
	}

	return errors.Join(errs...)
}
