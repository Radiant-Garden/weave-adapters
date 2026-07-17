// Package httpserver provides the adapter HTTP server lifecycle: it mounts the
// standard endpoints around adapter-provided routes and handles graceful
// shutdown.
//
// This is the M1 walking-skeleton form — it mounts only /api/v1/health. Later
// phases add /metrics, middleware, and configurable timeouts.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 15 * time.Second
)

// Server wraps net/http.Server with the adapter's standard routes.
type Server struct {
	httpServer *http.Server
}

// New builds a Server listening on addr with the given health handler mounted
// at GET /api/v1/health.
func New(addr string, healthHandler http.Handler) *Server {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/health", healthHandler)

	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
}

// Run starts the server and blocks until ctx is cancelled, then drains
// in-flight requests within the shutdown grace period. It emits SYS lifecycle
// events and returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		events.Emit(ctx, catalog.SYS002, "addr", s.httpServer.Addr)

		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
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

		err := s.httpServer.Shutdown(shutdownCtx)

		events.Emit(shutdownCtx, catalog.SYS004)

		return err
	}
}
