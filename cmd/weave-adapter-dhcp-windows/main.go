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

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
	"github.com/radiantgarden/weave-adapters/internal/core/observability"
)

// version is the adapter version, overridable via -ldflags at build time.
var version = "0.0.0-dev"

// main owns the two things run must not: signal wiring and the process exit
// code. It is the only place that calls os.Exit, so every startup path stays
// testable through run.
func main() {
	// On Windows Server 2022 a console exe receives os.Interrupt (Ctrl+C,
	// CTRL_CLOSE); SIGTERM is a no-op there but keeps Unix dev parity.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := run(ctx, os.Args[1:])

	// Not deferred: os.Exit below would skip it.
	stop()

	if err != nil {
		// Startup failures are operational outcomes, not stray slog calls.
		// observability.Setup may not have run yet; the events system writes to
		// slog.Default either way.
		events.Emit(context.Background(), catalog.SYS005, "error", err.Error())
		os.Exit(1)
	}
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

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := httpserver.New(addr, health.NewHandler(version, started))

	return srv.Run(ctx)
}
