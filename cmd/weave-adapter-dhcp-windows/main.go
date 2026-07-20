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
)

// version is the adapter version, overridable via -ldflags at build time.
var version = "0.0.0-dev"

// main owns the three things run must not: signal wiring, the CLI-vs-server
// split, and the process exit code. It is the only place that calls os.Exit, so
// every startup path stays testable through run.
func main() {
	args := os.Args[1:]

	var err error

	if isTokenCommand(args) {
		err = runToken(args[1:], os.Stdout, time.Now)
		if err != nil {
			// A CLI mistake (bad flag, duplicate label) is not a startup
			// failure: it gets a plain message on stderr, never a SYS-005
			// event. Emitting one would hand an operator who typo'd a flag a
			// structured log line claiming the adapter failed to start.
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	} else {
		err = runServer(args)
	}

	if err != nil {
		os.Exit(1)
	}
}

// isTokenCommand reports whether args invoke token management rather than a
// server run.
func isTokenCommand(args []string) bool {
	return len(args) > 0 && args[0] == "token"
}

// runServer runs the adapter until a signal arrives, reporting a startup
// failure as SYS-005.
func runServer(args []string) error {
	// On Windows Server 2022 a console exe receives os.Interrupt (Ctrl+C,
	// CTRL_CLOSE); SIGTERM is a no-op there but keeps Unix dev parity.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := run(ctx, args)

	// Undeferred so the signal handler is released before the SYS-005 emit
	// below, rather than staying installed across shutdown reporting.
	stop()

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
func run(ctx context.Context, args []string) error {
	// Taken before any work so uptime measures the process, not the server.
	started := time.Now()

	// The spec is composed here, at the one place that knows which adapter this
	// binary is. Core owns the precedence machinery, never the key set.
	values, err := config.Load(append(config.CoreKeys(), dhcpwindows.Keys()...), args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Both halves are built from one resolved set, and their errors are joined
	// so an operator sees every problem in one run rather than one per restart.
	cfg, coreErr := config.Core(values)
	adapterCfg, adapterErr := dhcpwindows.NewConfig(values)

	if err := errors.Join(coreErr, adapterErr); err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	observability.Setup(cfg.LogSeverity)

	// Importing the catalog package registers the core events from init(), which
	// panics on a contract violation — so by this line the catalog is known good.
	events.Emit(ctx, catalog.SYS001, "version", version)

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
		return err
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
	scopes := etag.Conditional(dhcpwindows.NewScopesHandler(backend, adapterCfg))

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := httpserver.New(addr, health.NewHandler(version, started, probe),
		httpserver.WithInnerMiddleware(authMiddleware...),
		httpserver.WithOpenAPISpec(apispec.Spec()),
		httpserver.WithRoutes(httpserver.Route{
			Pattern: "GET " + dhcpwindows.ScopesPath,
			Handler: scopes,
		}),
	)

	return srv.Run(ctx)
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
				verifier.Len(), cfg.AuthTokensFile)
		}

		return nil, fmt.Errorf("no tokens configured in %q: run `token gen --label <name>` or set disableAuth",
			cfg.AuthTokensFile)
	}

	return []middleware.Middleware{auth.Bearer(verifier, httpserver.Unauthenticated)}, nil
}
