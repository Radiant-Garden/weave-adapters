// Command weave-adapter-dhcp-windows is the REST adapter that will expose
// Windows Server DHCP behind the uniform weave-adapters HTTP API.
//
// M1 walking skeleton: it serves GET /api/v1/health and nothing else — it does
// not talk to DHCP in any form. Structured logging, events, metrics, and
// middleware are layered in during later M1 phases; for now logging uses the
// stdlib logger.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
)

// version is the adapter version, overridable via -ldflags at build time.
var version = "0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := httpserver.New(addr, health.NewHandler(version, time.Now()))

	//nolint:gosec // G706: cfg.Port is a validated int (1-65535) rendered with %d and cannot carry a log-injection payload. Temporary stdlib log; replaced by structured events in Phase 2.
	log.Printf("weave-adapter-dhcp-windows %s listening on port %d (health: /api/v1/health)", version, cfg.Port)

	return srv.Run(ctx)
}
