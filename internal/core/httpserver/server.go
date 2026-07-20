// Package httpserver provides the adapter HTTP server lifecycle: it mounts the
// standard endpoints around adapter-provided routes and handles graceful
// shutdown.
//
// It mounts /api/v1/health and /openapi.yaml itself, and takes everything
// adapter-specific — the resource routes, the spec document, the inner
// middleware — as values through Option. That direction is not stylistic: core
// must never import internal/adapters, so anything an adapter owns has to
// arrive as a parameter rather than as an import.
package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/middleware"
)

const (
	// readHeaderTimeout bounds how long a client may take to send the request
	// line and headers. It is the first line of defence against a Slowloris that
	// dribbles headers to pin a connection.
	readHeaderTimeout = 10 * time.Second

	// readTimeout bounds the whole request read, headers plus body. The bodies
	// this API accepts are single resources of a few hundred bytes, so a read
	// that takes tens of seconds is a dripped body rather than a large one, and
	// bounding it cannot truncate an honest request. Handlers read the body
	// before they touch the backend, so this deadline never fires mid-handler on
	// a slow backend call.
	readTimeout = 30 * time.Second

	// idleTimeout bounds a kept-alive connection between requests. Without it a
	// client can hold a connection open indefinitely after one request, so a
	// handful of clients can occupy every connection slot while sending nothing.
	// It has no bearing on an active request, so no legitimate slow response is
	// at risk.
	idleTimeout = 120 * time.Second

	healthPath  = "/api/v1/health"
	openAPIPath = "/openapi.yaml"

	// openAPIContentType is the media type RFC 9512 registers for YAML. The
	// document is served as the bytes on disk rather than re-serialized, so a
	// client and a reviewer read the same file.
	openAPIContentType = "application/yaml"
)

// shutdownGrace bounds the drain. A var rather than a const so the drain-overran
// path can be tested without a test that sleeps for the real grace period; no
// production code assigns it.
var shutdownGrace = 15 * time.Second

// ErrShutdownIncomplete reports that the drain grace period expired with
// requests still in flight. It exists so a caller can tell a failed shutdown
// after hours of serving from a failure to start at all — the two want opposite
// operator responses, and both arrive as a non-nil error from Run.
var ErrShutdownIncomplete = errors.New("shutdown did not complete within the grace period")

// Server wraps net/http.Server with the adapter's standard routes.
type Server struct {
	httpServer *http.Server
}

// Route is one adapter-supplied route: a net/http.ServeMux pattern and the
// handler that answers it.
//
// A pattern carries its method ("GET /api/v1/scopes"), because a bare path
// matches every method and would turn a POST to a read-only collection into a
// 200 rather than the 405 the error conventions promise.
type Route struct {
	Pattern string
	Handler http.Handler
}

// Option configures the server New builds. Every adapter-owned input arrives
// this way — see the package doc for why it cannot arrive as an import.
type Option func(*settings)

// settings is the resolved option set. Unexported: an adapter composes it
// through the With* functions, so a field added here is not a breaking change
// at any call site.
type settings struct {
	inner        []middleware.Middleware
	routes       []Route
	spec         []byte
	writeTimeout time.Duration
}

// WithInnerMiddleware adds middlewares inside the standard chain —
// authentication in practice. They run after logging so a rejected request
// still produces its API-010 audit line, and they see the caller context the
// request-ID middleware established.
func WithInnerMiddleware(mw ...middleware.Middleware) Option {
	return func(s *settings) { s.inner = append(s.inner, mw...) }
}

// WithRoutes mounts adapter resource routes on the server's mux, inside the
// same middleware chain the standard endpoints run behind.
//
// Every pattern is checked by validateRoute before it is mounted, and a bad one
// panics at process start rather than serving something the operator did not
// configure.
func WithRoutes(routes ...Route) Option {
	return func(s *settings) { s.routes = append(s.routes, routes...) }
}

// validateRoute panics unless pattern is a method-qualified pattern that leaves
// the standard endpoints alone.
//
// http.ServeMux's own conflict detection is not enough here, and the gap is not
// obvious. It rejects an exact duplicate of "GET /api/v1/health" — but a bare
// "/api/v1/health" is a *different* pattern that does not conflict, so it
// registers happily and then answers every method the standard route did not
// claim. Health keeps answering GET while POST and DELETE quietly reach the
// adapter, replacing the problem+json 405 the error conventions promise. A bare
// "/" is worse: it matches every unrouted path, so the API-900 404 stops firing
// at all and an operator sees no signal for either.
//
// Requiring the method is what closes both. It is also the rule the API needs
// anyway — a path-only pattern turns a POST to a read-only collection into a
// 200 — so this enforces a convention rather than adding one.
func validateRoute(pattern string) {
	method, path, hasMethod := strings.Cut(pattern, " ")
	if !hasMethod || method == "" || !strings.HasPrefix(path, "/") {
		panic(fmt.Sprintf(
			"httpserver: route pattern %q must be method-qualified, e.g. \"GET /api/v1/scopes\"; "+
				"a path-only pattern matches every method and would answer 200 where the API promises 405",
			pattern))
	}

	// Reserved paths are rejected by path, not by whole pattern: it is the
	// method-qualified variants that ServeMux would let through silently.
	if path == healthPath || path == openAPIPath {
		panic(fmt.Sprintf(
			"httpserver: route pattern %q targets %s, which this package owns; "+
				"an adapter route there would shadow the endpoint weave polls",
			pattern, path))
	}

	// A catch-all matches every path the standard routes did not claim, so the
	// mux stops generating the 404 that ProblemErrors renders as problem+json —
	// every unknown path would answer with the adapter's handler instead.
	if path == "/" {
		panic(fmt.Sprintf(
			"httpserver: route pattern %q is a catch-all; it would swallow the router's own 404, "+
				"which is where the problem+json error shape for unknown paths comes from",
			pattern))
	}
}

// WithOpenAPISpec serves spec as the document at GET /openapi.yaml.
//
// Without it the route answers a problem+json 404, which is what an adapter
// that has not written its spec yet should say, and what keeps this package
// testable without one.
//
// The bytes are copied. What the server hands to every client for the life of
// the process should not be an alias of a slice the caller still holds — a
// package-level []byte from go:embed is the obvious source, and it is writable
// by anything that can see it.
func WithOpenAPISpec(spec []byte) Option {
	return func(s *settings) { s.spec = bytes.Clone(spec) }
}

// WithWriteTimeout bounds how long the server may take to write a response,
// measured from the end of the request headers and therefore across the whole
// handler run — the backend call included.
//
// It arrives as an Option rather than a constant because this package is
// adapter-agnostic and the safe value is not: a WriteTimeout below an adapter's
// own backend timeout would truncate a slow-but-honest response and hand the
// client a torn body instead of the 502/504 the backend classification promises.
// Only the binary knows that timeout, so the binary sets this to comfortably
// exceed it. Zero — the default — leaves the write unbounded, which is the
// pre-existing behaviour and keeps a server built without the option serveable.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *settings) { s.writeTimeout = d }
}

// New builds a Server listening on addr with the given health handler mounted
// at GET /api/v1/health, wrapped in the standard middleware chain (recovery →
// request-ID → logging → inner → problem-errors).
func New(addr string, healthHandler http.Handler, opts ...Option) *Server {
	var set settings

	for _, opt := range opts {
		opt(&set)
	}

	mux := http.NewServeMux()
	mux.Handle("GET "+healthPath, healthHandler)
	mux.HandleFunc("GET "+openAPIPath, serveOpenAPI(set.spec))

	// Validated then mounted, and after the standard routes: validateRoute
	// catches the patterns ServeMux would accept and silently shadow with, and
	// ServeMux itself still catches an exact duplicate of another adapter route.
	for _, route := range set.routes {
		validateRoute(route.Pattern)
		mux.Handle(route.Pattern, route.Handler)
	}

	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           NewHandler(mux, set.inner...),
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			IdleTimeout:       idleTimeout,
			// Zero unless the binary set it — see WithWriteTimeout for why the
			// safe bound is the adapter's to choose.
			WriteTimeout: set.writeTimeout,
		},
	}
}

// NewHandler wraps router in the adapter's standard middleware chain. See New
// for the ordering and why inner sits where it does.
//
// It is exported so a caller mounting its own routes runs the same chain the
// server does rather than assembling a copy — M2's demo resource is the first,
// and a lookalike chain would prove nothing about the real one.
func NewHandler(router http.Handler, inner ...middleware.Middleware) http.Handler {
	standard := []middleware.Middleware{
		middleware.Recovery,
		middleware.RequestID,
		// Health is polled frequently; recover and correlate it, but don't emit
		// an API-010 audit line for every poll.
		middleware.Logging(skipHealthPolls),
	}

	// ProblemErrors is innermost so it wraps the router itself: the router
	// generates its own 404/405 responses, which no route-level code can reach.
	chain := slices.Concat(standard, inner, []middleware.Middleware{middleware.ProblemErrors})

	return middleware.Chain(router, chain...)
}

// skipHealthPolls reports whether request logging should skip this request.
//
// Health GETs are skipped when they answer normally — 200 healthy/unhealthy or
// 503 unavailable are both routine, and weave polls continuously. Logging the
// 503s would put out thousands of identical lines per hour during exactly the
// outage an operator is trying to read the log through, and HLT-001 already
// records the transition once.
//
// Anything else on that path is audited: a 405 from a misconfigured client or a
// 500 from a broken probe is not a poll, and suppressing the whole path would
// hide the requests there most worth seeing.
func skipHealthPolls(r *http.Request, status int) bool {
	if r.URL.Path != healthPath || r.Method != http.MethodGet {
		return false
	}

	return status == http.StatusOK || status == http.StatusServiceUnavailable
}

// Unauthenticated reports whether r addresses a route that must stay open:
// health, because weave polls it to decide whether the adapter is reachable at
// all and an auth failure there would read as an outage, and the spec route,
// which describes the API rather than exposing any of its data.
//
// Everything else authenticates, including paths that match no route — an
// unauthenticated caller learns nothing about which routes exist.
func Unauthenticated(r *http.Request) bool {
	return r.URL.Path == healthPath || r.URL.Path == openAPIPath
}

// serveOpenAPI answers the spec route with spec, or with a 404 when the caller
// supplied no document.
//
// The absent case goes through apierror rather than http.Error so the body is
// problem+json like every other error this API returns — 03-api-conventions
// requires that shape "including 401/404 from middleware", and a plain-text 404
// here would be the one response a generic client could not parse.
//
// The document is written as bytes rather than parsed and re-emitted: the
// contract a client reads must be the file a reviewer reads, and a round trip
// through a YAML library would reorder keys and drop comments.
func serveOpenAPI(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(spec) == 0 {
			apierror.WriteError(w, r, apierror.NotFound("openapi document"))

			return
		}

		w.Header().Set("Content-Type", openAPIContentType)

		// Set explicitly: the document is well past net/http's sniff buffer, so
		// without this it goes out chunked with no length for a client to size
		// the download against.
		w.Header().Set("Content-Length", strconv.Itoa(len(spec)))

		// The contract changes only when a new binary is deployed, so a client
		// polling it is re-fetching bytes it already has. no-cache rather than a
		// max-age: it still revalidates every time, which keeps a spec served
		// from behind a proxy from going stale across a deploy.
		w.Header().Set("Cache-Control", "no-cache")

		w.WriteHeader(http.StatusOK)

		// The response is already committed; a write failure is not actionable
		// here, and the request logging middleware records the status either way.
		_, _ = w.Write(spec)
	}
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

		shutdownErr := s.httpServer.Shutdown(shutdownCtx)

		// Shutdown closes the listeners, so Serve has returned or is about to
		// and this cannot block. Reading it is what keeps a genuine Serve
		// failure that raced the cancel from being reported as a clean exit:
		// this branch would otherwise leave the error in the buffered channel
		// and return whatever Shutdown said about an already-dead server.
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}

		if shutdownErr != nil {
			// Not SYS-004: the drain did not complete, and claiming it did is
			// the version of this line an operator would act on wrongly.
			events.Emit(shutdownCtx, catalog.SYS007,
				"error", shutdownErr.Error(),
				"graceSeconds", int(shutdownGrace.Seconds()),
			)

			return fmt.Errorf("%w: %w", ErrShutdownIncomplete, shutdownErr)
		}

		events.Emit(shutdownCtx, catalog.SYS004)

		return serveErr
	}
}
