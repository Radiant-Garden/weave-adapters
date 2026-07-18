/*
Testing: main.go

Pending:

Tested:
  run
    - TestRun_ShouldServeHealthUntilContextCancelled: the wired binary serves health and shuts down cleanly.
    - TestRun_ShouldReturnErrorWhenConfigInvalid: a bad flag value fails startup before anything binds.
    - TestRun_ShouldReturnErrorWhenPortUnavailable: a port conflict fails startup rather than serving.

Tested elsewhere:
  Each wired component (config.Load, observability.Setup, httpserver.New/Run,
  health.NewHandler) is unit-tested in its own package; the tests here cover only
  what wiring adds — that the pieces are connected in the right order and that
  every failure path returns instead of exiting.

Declined:
  main — signal.NotifyContext plus os.Exit cannot be exercised without either
  signalling or killing the test process. Hoisting the signal context out of run
  is what makes the rest of the startup path testable, and that split is the only
  logic main holds.

Additional Remarks:
  These tests call observability.Setup, which replaces the process-global default
  slog logger, so they cannot run in parallel.

  Tests bind a port discovered by the OS rather than a hard-coded one. Config
  validation rejects port 0, so the port cannot be left for the kernel to pick at
  bind time — there is a small window between discovering the port and run
  binding it.
*/

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
)

// freePort returns a port that is free right now, having closed the listener
// used to discover it.
func freePort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig

	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok, "TCP listener should report a *net.TCPAddr")
	require.NoError(t, listener.Close())

	return addr.Port
}

// waitForListening blocks until run has emitted SYS-002.
func waitForListening(t *testing.T, rec *eventstest.Recorder) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if len(rec.FindByID(catalog.SYS002)) > 0 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("server did not report listening within 5s")
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldServeHealthUntilContextCancelled(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	port := freePort(t)
	errCh := make(chan error, 1)

	go func() { errCh <- run(ctx, []string{"--port", strconv.Itoa(port)}) }()

	waitForListening(t, rec)

	// ACT
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/health"

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// ASSERT — the config port, health handler, and middleware chain are wired.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body health.Response
	require.NoError(t, json.Unmarshal(raw, &body))

	assert.Equal(t, health.StatusHealthy, body.Status)
	assert.Equal(t, version, body.Version)
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))

	// Cancellation stands in for Ctrl+C, which main translates from a signal.
	cancel()
	require.NoError(t, <-errCh)

	rec.AssertEmitted(t, catalog.SYS001)
	rec.AssertData(t, catalog.SYS001, "version", version)
	rec.AssertEmitted(t, catalog.SYS004)
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldReturnErrorWhenConfigInvalid(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT — out of range, so Load fails validation.
	err := run(t.Context(), []string{"--port", "70000"})

	// ASSERT — startup stops at config; nothing announces itself as running.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading config")
	assert.Contains(t, err.Error(), "port must be between")
	rec.AssertNotEmitted(t, catalog.SYS001)
	rec.AssertNotEmitted(t, catalog.SYS002)
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldReturnErrorWhenPortUnavailable(t *testing.T) {
	// ARRANGE — hold the port so run cannot bind it.
	var lc net.ListenConfig

	port := freePort(t)

	held, err := lc.Listen(t.Context(), "tcp", ":"+strconv.Itoa(port))
	require.NoError(t, err)

	t.Cleanup(func() { _ = held.Close() })

	// ACT
	err = run(t.Context(), []string{"--port", strconv.Itoa(port)})

	// ASSERT — a bind conflict is a startup error, not a silent no-op.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listening on")
}
