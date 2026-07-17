// Command weave-adapter-dhcp-windows is the REST adapter that will expose
// Windows Server DHCP behind the uniform weave-adapters HTTP API.
//
// M1 walking skeleton: it serves GET /api/v1/health and nothing else — it does
// not talk to DHCP in any form. Config, structured logging, events, metrics,
// and middleware are layered in during later M1 phases; for now the listen
// address is fixed and logging uses the stdlib logger.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
)

// version is the adapter version, overridable via -ldflags at build time.
var version = "0.0.0-dev"

// listenAddr is the fixed listen address until config lands in Phase 1.
const listenAddr = ":8444"

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := httpserver.New(listenAddr, health.NewHandler(version, time.Now()))

	log.Printf("weave-adapter-dhcp-windows %s listening on %s (health: /api/v1/health)", version, listenAddr)

	return srv.Run(ctx)
}
