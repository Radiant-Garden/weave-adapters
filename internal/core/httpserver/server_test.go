/*
Testing: server.go

Pending:

Tested:
  New
    - TestNew_ShouldServeHealthEndpoint: GET /api/v1/health returns the weave health shape.
    - TestNew_ShouldReturnNotFoundForOpenAPIDocument: the reserved spec route answers 404 in M1.
    - TestNew_ShouldEchoRequestIDHeader: the middleware chain is wired around the mux.
    - TestNew_ShouldRecoverFromHandlerPanic: a panicking handler yields 500, not a dead connection.
    - TestNew_ShouldSkipRequestLoggingForHealthPolls: health polls emit no API-010; other routes do.
  Run
    - TestRun_ShouldShutDownGracefullyWhenContextCancelled: returns nil and emits SYS-002/003/004.
    - TestRun_ShouldServeUntilContextCancelled: requests are served while Run blocks.
    - TestRun_ShouldReturnErrorWhenAddressUnavailable: a bind conflict errors without emitting SYS-002.

Tested elsewhere:
  serveOpenAPI, skipHealthPolls — exercised through New's routing tests rather
  than called directly; they only exist as part of the mounted chain.

Declined:

Additional Remarks:
  Run tests bind 127.0.0.1:0 so they never collide with a developer's ports or
  with each other, and drive shutdown via context cancel (not a real signal) so
  they behave identically on Windows and Unix.

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

	// ASSERT
	assert.Equal(t, http.StatusNotFound, resp.status)
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

	// ASSERT — the skip is scoped to health, not a blanket disable.
	rec.AssertEmittedN(t, catalog.API010, 1)
	// slog widens ints, so the recorded value comes back as int64.
	rec.AssertData(t, catalog.API010, "status", int64(http.StatusNotFound))
	rec.AssertMatchesCatalog(t)
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
