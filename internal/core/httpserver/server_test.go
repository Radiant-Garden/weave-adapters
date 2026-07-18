/*
Testing: server.go

Pending:

Tested:
  New
    - TestNew_ShouldServeHealthEndpoint: GET /api/v1/health returns the weave health shape.
    - TestNew_ShouldReturnNotFoundForOpenAPIDocument: the reserved spec route answers a problem+json 404.
    - TestNew_ShouldEchoRequestIDHeader: the middleware chain is wired around the mux.
    - TestNew_ShouldRecoverFromHandlerPanic: a panicking handler yields 500, not a dead connection.
    - TestNew_ShouldSkipRequestLoggingForHealthPolls: successful health polls emit no API-010; other routes and failures do.
    - TestNew_ShouldRenderRouterErrorsAsProblemJSON: mux 404/405 share the one error shape.
    - TestNew_ShouldLogRejectedRequests: inner middleware runs inside logging, so rejections are audited.
  NewHandler
    - Every New test above exercises it: New builds its handler by calling it,
      so the chain order, recovery, request-ID, logging-skip and problem+json
      assertions all run through NewHandler rather than around it.
  skipHealthPolls
    - TestSkipHealthPolls_ShouldSuppressOnlyRoutineAnswers: routine polls are quiet; failures on that path are not.
  Run
    - TestRun_ShouldShutDownGracefullyWhenContextCancelled: returns nil and emits SYS-002/003/004.
    - TestRun_ShouldServeUntilContextCancelled: requests are served while Run blocks.
    - TestRun_ShouldReturnErrorWhenAddressUnavailable: a bind conflict errors without emitting SYS-002.
    - TestRun_ShouldReportAnIncompleteShutdownRatherThanClaimItDrained: an overrun drain is SYS-007, never SYS-004.

Tested elsewhere:
  serveOpenAPI, skipHealthPolls — exercised through New's routing tests rather
  than called directly; they only exist as part of the mounted chain.

  NewHandler with a caller-supplied router: internal/core/httptest mounts the
  demo resource through it, which is the case New itself cannot cover since it
  always builds its own mux.

Declined:
  The Serve-error-racing-a-context-cancel path. Run now drains errCh after
  Shutdown so the error cannot be swallowed, but provoking that race
  deterministically means failing Serve at the instant of cancellation, which no
  seam here exposes. The read is unconditional, so the ordinary cancel tests
  above prove it does not deadlock; the raced error itself is unasserted.

Additional Remarks:
  Run tests bind 127.0.0.1:0 so they never collide with a developer's ports or
  with each other, and drive shutdown via context cancel (not a real signal) so
  they behave identically on Windows and Unix.

  TestRun_ShouldReportAnIncompleteShutdownRatherThanClaimItDrained overrides the
  package-level shutdownGrace so it costs 50ms rather than the real 15s. That is
  why the var exists; nothing in production assigns it.

  Tests that install the event recorder mutate the process-global emitter hook
  and therefore cannot run in parallel.
*/

package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
)

// newTestServer returns an httptest server running the full handler New builds,
// so routing and the middleware chain are exercised end to end.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := New(":0", health.NewHandler("1.2.3", time.Now()))

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	return ts
}

// response is the part of an HTTP response these tests assert on, captured once
// the body has been drained and closed. Returning this rather than an
// *http.Response keeps body ownership inside the helper.
type response struct {
	status int
	header http.Header
	body   []byte
}

// get performs a GET against ts (sending header, which may be nil) and returns
// the captured response.
func get(t *testing.T, ts *httptest.Server, path string, header http.Header) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)

	maps.Copy(req.Header, header)

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

func TestNew_ShouldServeHealthEndpoint(t *testing.T) {
	t.Parallel()

	// ARRANGE
	ts := newTestServer(t)

	// ACT
	resp := get(t, ts, "/api/v1/health", nil)

	// ASSERT
	require.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, "application/json", resp.header.Get("Content-Type"))

	var body health.Response
	require.NoError(t, json.Unmarshal(resp.body, &body))

	assert.Equal(t, health.StatusHealthy, body.Status)
	assert.Equal(t, "1.2.3", body.Version)
	assert.NotEmpty(t, body.Components)
}

func TestNew_ShouldReturnNotFoundForOpenAPIDocument(t *testing.T) {
	t.Parallel()

	// ARRANGE
	ts := newTestServer(t)

	// ACT — the route is reserved in M1; the document itself arrives in M2.
	resp := get(t, ts, "/openapi.yaml", nil)

	// ASSERT — problem+json, not stdlib plain text: 03-api-conventions requires
	// every error share one shape, including 404s.
	require.Equal(t, http.StatusNotFound, resp.status)
	assert.Equal(t, "application/problem+json", resp.header.Get("Content-Type"))

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.body, &problem))
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
	assert.Equal(t, "/openapi.yaml", problem.Instance)
	assert.NotEmpty(t, problem.RequestID)
}

func TestNew_ShouldEchoRequestIDHeader(t *testing.T) {
	t.Parallel()

	// ARRANGE
	ts := newTestServer(t)

	header := http.Header{}
	header.Set("X-Request-Id", "caller-supplied-id")

	// ACT
	resp := get(t, ts, "/api/v1/health", header)

	// ASSERT — proves the chain wraps the mux, not just that RequestID works.
	assert.Equal(t, "caller-supplied-id", resp.header.Get("X-Request-Id"))
}

func TestNew_ShouldRecoverFromHandlerPanic(t *testing.T) {
	t.Parallel()

	// ARRANGE — a health handler standing in for any route that blows up.
	panicking := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	srv := New(":0", panicking)

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	// ACT
	resp := get(t, ts, "/api/v1/health", nil)

	// ASSERT — recovery is outermost, so the client gets a response.
	assert.Equal(t, http.StatusInternalServerError, resp.status)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestNew_ShouldSkipRequestLoggingForHealthPolls(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ts := newTestServer(t)

	// ACT — health is polled constantly; /openapi.yaml is an ordinary request.
	get(t, ts, "/api/v1/health", nil)
	rec.AssertNotEmitted(t, catalog.API010)

	get(t, ts, "/openapi.yaml", nil)

	// A non-GET on the health path is not a poll, and must still be audited.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/health", nil)
	require.NoError(t, err)

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	// ASSERT — the skip is scoped to successful health polls, not the path.
	// The openapi 404 and the health 405 each emit their own error event too;
	// only the request-audit lines are counted here.
	rec.AssertEmittedN(t, catalog.API010, 2)
	// slog widens ints, so the recorded value comes back as int64.
	rec.AssertData(t, catalog.API010, "status", int64(http.StatusNotFound))
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestSkipHealthPolls_ShouldSuppressOnlyRoutineAnswers(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		status   int
		wantSkip bool
	}{
		{
			name: "should skip a healthy poll", method: http.MethodGet, path: "/api/v1/health",
			status: http.StatusOK, wantSkip: true,
		},
		{
			// The outage is when poll volume matters most and the information
			// is least new — HLT-001 already recorded the transition once.
			name: "should skip an unavailable poll", method: http.MethodGet, path: "/api/v1/health",
			status: http.StatusServiceUnavailable, wantSkip: true,
		},
		{
			name: "should log a wrong method on the health path", method: http.MethodPost, path: "/api/v1/health",
			status: http.StatusMethodNotAllowed, wantSkip: false,
		},
		{
			name: "should log an unexpected failure on the health path", method: http.MethodGet, path: "/api/v1/health",
			status: http.StatusInternalServerError, wantSkip: false,
		},
		{
			name: "should log every other path", method: http.MethodGet, path: "/api/v1/leases",
			status: http.StatusOK, wantSkip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)

			// ACT / ASSERT
			assert.Equal(t, tt.wantSkip, skipHealthPolls(req, tt.status))
		})
	}
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestNew_ShouldRenderRouterErrorsAsProblemJSON(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantType   string
		wantEvent  events.EventID
	}{
		{
			name: "should render an unmatched route as problem+json", method: http.MethodGet, path: "/api/v1/nope",
			wantStatus: http.StatusNotFound, wantType: "weave-adapters:not-found", wantEvent: catalog.API900,
		},
		{
			name: "should render a wrong method as problem+json", method: http.MethodPost, path: "/api/v1/health",
			wantStatus: http.StatusMethodNotAllowed, wantType: "weave-adapters:method-not-allowed", wantEvent: catalog.API902,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			ts := newTestServer(t)

			// ACT
			req, err := http.NewRequestWithContext(t.Context(), tt.method, ts.URL+tt.path, nil)
			require.NoError(t, err)

			resp, err := ts.Client().Do(req)
			require.NoError(t, err)

			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			// ASSERT — through the real chain, not just the middleware alone.
			require.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, apierror.ContentType, resp.Header.Get("Content-Type"))

			var problem apierror.Problem
			require.NoError(t, json.Unmarshal(body, &problem))
			assert.Equal(t, tt.wantType, problem.Type)
			assert.NotEmpty(t, problem.RequestID)

			rec.AssertEmitted(t, tt.wantEvent)
			rec.AssertMatchesCatalog(t)

			// The audit line is still emitted for a rejected request.
			rec.AssertEmittedN(t, catalog.API010, 1)
		})
	}
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestNew_ShouldLogRejectedRequests(t *testing.T) {
	// ARRANGE — an inner middleware standing in for auth, which rejects before
	// the mux is reached.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	reject := func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apierror.WriteError(w, r, apierror.NotFound("stand-in for auth"))
		})
	}

	srv := New(":0", health.NewHandler("1.2.3", time.Now()), reject)

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	// ACT
	get(t, ts, "/api/v1/leases", nil)

	// ASSERT — inner middleware runs inside logging, so a rejection is still
	// audited. Ordering this the other way would lose every rejected request
	// from the audit trail.
	rec.AssertEmittedN(t, catalog.API010, 1)
	rec.AssertData(t, catalog.API010, "status", int64(http.StatusNotFound))
	rec.AssertEmitted(t, catalog.API900)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestRun_ShouldShutDownGracefullyWhenContextCancelled(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(t.Context())
	srv := New("127.0.0.1:0", health.NewHandler("1.2.3", time.Now()))
	errCh := make(chan error, 1)

	go func() { errCh <- srv.Run(ctx) }()

	// ACT — cancel stands in for SIGINT/Ctrl+C, which keeps this OS-portable.
	cancel()

	// ASSERT
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation")
	}

	rec.AssertEmitted(t, catalog.SYS002)
	rec.AssertEmitted(t, catalog.SYS003)
	rec.AssertEmitted(t, catalog.SYS004)
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestRun_ShouldServeUntilContextCancelled(t *testing.T) {
	// ARRANGE — capture SYS-002 to learn the port the kernel actually assigned.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	srv := New("127.0.0.1:0", health.NewHandler("1.2.3", time.Now()))
	errCh := make(chan error, 1)

	go func() { errCh <- srv.Run(ctx) }()

	addr := waitForListenAddr(t, rec)

	// ACT
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/api/v1/health", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	// ASSERT
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()
	require.NoError(t, <-errCh)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestRun_ShouldReturnErrorWhenAddressUnavailable(t *testing.T) {
	// ARRANGE — hold the port so the server cannot bind it.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	var lc net.ListenConfig

	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = held.Close() })

	srv := New(held.Addr().String(), health.NewHandler("1.2.3", time.Now()))

	// ACT
	err = srv.Run(t.Context())

	// ASSERT — a bind failure is a startup error, never a "listening" event.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listening on "+held.Addr().String())
	rec.AssertNotEmitted(t, catalog.SYS002)
}

//nolint:paralleltest // installs the recorder and overrides shutdownGrace, both global
func TestRun_ShouldReportAnIncompleteShutdownRatherThanClaimItDrained(t *testing.T) {
	// ARRANGE — a handler that outlives the grace period, which is what a drain
	// timeout means in production: a request still in flight when time runs out.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	original := shutdownGrace
	shutdownGrace = 50 * time.Millisecond

	t.Cleanup(func() { shutdownGrace = original })

	inFlight := make(chan struct{})
	release := make(chan struct{})

	t.Cleanup(func() { close(release) })

	blocking := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(inFlight)
			<-release
			next.ServeHTTP(w, r)
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	srv := New("127.0.0.1:0", health.NewHandler("1.2.3", time.Now()), blocking)
	errCh := make(chan error, 1)

	go func() { errCh <- srv.Run(ctx) }()

	addr := waitForListenAddr(t, rec)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+healthPath, nil)
	require.NoError(t, err)

	go func() {
		resp, reqErr := http.DefaultClient.Do(req)
		if reqErr == nil {
			_ = resp.Body.Close()
		}
	}()

	<-inFlight

	// ACT — the drain starts with that request still running and cannot finish.
	cancel()

	// ASSERT
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrShutdownIncomplete)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation")
	}

	// The whole point: an operator filtering for SYS-004 must not see this
	// process report that it "drained cleanly" when it cut a request off.
	rec.AssertEmitted(t, catalog.SYS003)
	rec.AssertEmitted(t, catalog.SYS007)
	rec.AssertNotEmitted(t, catalog.SYS004)
	rec.AssertMatchesCatalog(t)
}

// waitForListenAddr blocks until Run has emitted SYS-002 and returns the
// resolved listen address from it.
func waitForListenAddr(t *testing.T, rec *eventstest.Recorder) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if listening := rec.FindByID(catalog.SYS002); len(listening) > 0 {
			addr, ok := listening[0].Data("addr").(string)
			require.True(t, ok, "SYS-002 addr field should be a string")

			return addr
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("server did not report a listen address within 5s")

	return ""
}
