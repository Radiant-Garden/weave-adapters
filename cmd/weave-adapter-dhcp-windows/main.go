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
	"log/slog"
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		// observability.Setup may not have run yet; slog.Default still logs.
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	observability.Setup(cfg.LogSeverity)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	events.Emit(ctx, catalog.SYS001, "version", version)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := httpserver.New(addr, health.NewHandler(version, time.Now()))

	return srv.Run(ctx)
}
