// Package httpserver provides the adapter HTTP server lifecycle: it mounts the
// standard endpoints around adapter-provided routes and handles graceful
// shutdown.
//
// This is the M1 form — it mounts /api/v1/health behind the standard middleware
// chain and reserves /openapi.yaml. Later phases add /metrics, the real OpenAPI
// document, and configurable timeouts.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/middleware"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 15 * time.Second

	healthPath  = "/api/v1/health"
	openAPIPath = "/openapi.yaml"
)

// Server wraps net/http.Server with the adapter's standard routes.
type Server struct {
	httpServer *http.Server
}

// New builds a Server listening on addr with the given health handler mounted
// at GET /api/v1/health, wrapped in the standard middleware chain (recovery →
// request-ID → logging).
func New(addr string, healthHandler http.Handler) *Server {
	mux := http.NewServeMux()
	mux.Handle("GET "+healthPath, healthHandler)
	mux.HandleFunc("GET "+openAPIPath, serveOpenAPI)

	handler := middleware.Chain(mux,
		middleware.Recovery,
		middleware.RequestID,
		// Health is polled frequently; recover and correlate it, but don't emit
		// an API-010 audit line for every poll.
		middleware.Logging(skipHealthPolls),
	)

	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
}

// skipHealthPolls reports whether request logging should skip r (the health
// endpoint).
func skipHealthPolls(r *http.Request) bool {
	return r.URL.Path == healthPath
}

// serveOpenAPI answers the reserved spec route. The document itself is M2
// (spec-first work); until then the route exists so it is owned and logged, and
// answers the way any absent document would.
func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "openapi document not available in this build", http.StatusNotFound)
}

// Run binds the listen address, serves, and blocks until ctx is cancelled, then
// drains in-flight requests within the shutdown grace period. It emits SYS
// lifecycle events and returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	// Bind before announcing: a port conflict must surface as a startup error,
	// not as a SYS-002 "listening" line followed by a failure.
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.httpServer.Addr, err)
	}

	// The resolved address, not the configured one — port 0 picks a real port.
	events.Emit(ctx, catalog.SYS002, "addr", listener.Addr().String())

	errCh := make(chan error, 1)

	go func() {
		errCh <- s.httpServer.Serve(listener)
	}()

	select {
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case <-ctx.Done():
		events.Emit(ctx, catalog.SYS003)

		// Background (not ctx): ctx is already cancelled, so the drain must run
		// on a fresh deadline or Shutdown would return immediately.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		err = s.httpServer.Shutdown(shutdownCtx)

		events.Emit(shutdownCtx, catalog.SYS004)

		return err
	}
}
