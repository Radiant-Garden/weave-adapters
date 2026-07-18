// Package httpserver provides the adapter HTTP server lifecycle: it mounts the
// standard endpoints around adapter-provided routes and handles graceful
// shutdown.
//
// This is the M1 form — it mounts /api/v1/health behind the standard middleware
// chain. Later phases add /metrics, /openapi.yaml, and configurable timeouts.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/middleware"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 15 * time.Second

	healthPath = "/api/v1/health"
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
