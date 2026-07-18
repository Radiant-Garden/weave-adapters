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
	"slices"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
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
// request-ID → logging → inner → problem-errors).
//
// inner are middlewares applied inside the standard chain — authentication in
// practice. They run after logging so a rejected request still produces its
// API-010 audit line, and they see the caller context the request-ID middleware
// established.
func New(addr string, healthHandler http.Handler, inner ...middleware.Middleware) *Server {
	mux := http.NewServeMux()
	mux.Handle("GET "+healthPath, healthHandler)
	mux.HandleFunc("GET "+openAPIPath, serveOpenAPI)

	standard := []middleware.Middleware{
		middleware.Recovery,
		middleware.RequestID,
		// Health is polled frequently; recover and correlate it, but don't emit
		// an API-010 audit line for every poll.
		middleware.Logging(skipHealthPolls),
	}

	// ProblemErrors is innermost so it wraps the mux itself: the router
	// generates its own 404/405 responses, which no route-level code can reach.
	chain := slices.Concat(standard, inner, []middleware.Middleware{middleware.ProblemErrors})

	handler := middleware.Chain(mux, chain...)

	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
}

// skipHealthPolls reports whether request logging should skip this request.
//
// Only a *successful* health GET is skipped. Suppressing the whole path would
// also hide a 405 from a misconfigured client or a 503 from a failing probe —
// the requests on that path most worth auditing.
func skipHealthPolls(r *http.Request, status int) bool {
	return r.URL.Path == healthPath && r.Method == http.MethodGet && status == http.StatusOK
}

// Unauthenticated reports whether r addresses a route that must stay open:
// health, because weave polls it to decide whether the adapter is reachable at
// all and an auth failure there would read as an outage, and the reserved spec
// route, which carries nothing worth protecting.
//
// Everything else authenticates, including paths that match no route — an
// unauthenticated caller learns nothing about which routes exist.
func Unauthenticated(r *http.Request) bool {
	return r.URL.Path == healthPath || r.URL.Path == openAPIPath
}

// serveOpenAPI answers the reserved spec route. The document itself is M2
// (spec-first work); until then the route exists so it is owned and logged, and
// answers the way any absent document would.
//
// It goes through apierror rather than http.Error so the body is problem+json
// like every other error this API returns — 03-api-conventions requires that
// shape "including 401/404 from middleware", and a plain-text 404 here would be
// the one response a generic client could not parse.
func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	apierror.WriteError(w, r, apierror.NotFound("openapi document"))
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
