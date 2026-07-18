// Command weave-adapter-dhcp-windows is the REST adapter that will expose
// Windows Server DHCP behind the uniform weave-adapters HTTP API.
//
// M1 walking skeleton: it serves GET /api/v1/health and nothing else — it does
// not talk to DHCP in any form. Logging goes through the cataloged events
// system (see internal/core/events); the HTTP server emits its own lifecycle
// events, so main only marks startup.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
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

	// Not deferred: main's os.Exit would skip it.
	stop()

	if err != nil {
		// Startup failures are operational outcomes, not stray slog calls.
		// observability.Setup may not have run yet; the events system writes to
		// slog.Default either way.
		events.Emit(context.Background(), catalog.SYS005, "error", err.Error())
	}

	return err
}

// run wires the adapter together and serves until ctx is cancelled. It returns
// errors rather than exiting so the whole startup path can be driven from tests.
func run(ctx context.Context, args []string) error {
	// Taken before any work so uptime measures the process, not the server.
	started := time.Now()

	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	observability.Setup(cfg.LogSeverity)

	// Importing the catalog package registers the core events from init(), which
	// panics on a contract violation — so by this line the catalog is known good.
	events.Emit(ctx, catalog.SYS001, "version", version)

	authMiddleware, err := buildAuth(ctx, cfg)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := httpserver.New(addr, health.NewHandler(version, started), authMiddleware...)

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
	if verifier.Len() == 0 {
		// An empty allow-list would reject every request, which looks like a
		// bug to whoever is on call. Fail at startup, where the message can say
		// what to do.
		return nil, fmt.Errorf("no tokens configured in %q: run `token gen --label <name>` or set disableAuth",
			cfg.AuthTokensFile)
	}

	return []middleware.Middleware{auth.Bearer(verifier, httpserver.Unauthenticated)}, nil
}
