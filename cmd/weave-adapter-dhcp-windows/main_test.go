/*
Testing: main.go

Pending:

Tested:
  run
    - TestRun_ShouldServeHealthUntilContextCancelled: the wired binary serves health and shuts down cleanly.
    - TestRun_ShouldReturnErrorWhenConfigInvalid: a bad flag value fails startup before anything binds.
    - TestRun_ShouldReturnErrorWhenPortUnavailable: a port conflict fails startup rather than serving.
    - TestRun_ShouldRefuseToStartWithoutTokens: a missing, empty, or wholly expired store fails startup with a fix.
    - TestRun_ShouldWarnLoudlyWhenAuthIsDisabled: starting wide open emits SYS-006.

  isTokenCommand
    - TestIsTokenCommand_ShouldRecogniseOnlyTheTokenVerb: server args never route to the CLI.

Tested elsewhere:
  Each wired component (config.Load, observability.Setup, httpserver.New/Run,
  health.NewHandler, auth.Bearer) is unit-tested in its own package; the tests
  here cover only what wiring adds — that the pieces are connected in the right
  order and that every failure path returns instead of exiting.

  runServer
    - TestRunServer_ShouldTreatHelpAsSuccess: --help exits 0, matching `token gen --help`.

Declined:
  main — os.Exit cannot be exercised without killing the test process. The
  CLI-vs-server split it performs is covered by isTokenCommand, and everything
  downstream is covered by run and runToken.

  runServer's signal path — signal.NotifyContext needs a real signal to
  exercise; the smoke tests cover it. Its error-classification branches (help,
  ErrShutdownIncomplete, SYS-005) are reachable without one.

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
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
)

// withIdentity appends the two identity inputs every server run now needs.
//
// identity.namespaceKey and identity.serverName are provisioned config with no
// defaults and no fallbacks, by design: an auto-generated namespace key
// regenerating on reinstall *is* a fleet-wide re-key, and a server name
// following os.Hostname() re-keys on a host rename. The adapter refuses to
// start without them, so every run here carries them exactly as a real
// deployment does.
func withIdentity(args ...string) []string {
	return append(args,
		"--identity-namespace-key", "main-test-namespace-key-0123456789",
		"--identity-server-name", "dhcp01.test",
	)
}

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

// tokenStore writes a token file holding one token and returns the path and the
// token itself, so a server test can authenticate for real.
func tokenStore(t *testing.T) (path, token string) {
	t.Helper()

	path = filepath.Join(t.TempDir(), "tokens.toml")

	token, err := auth.Generate()
	require.NoError(t, err)

	store := &auth.Store{Tokens: []auth.Entry{{
		Label:     "test-caller",
		Hash:      auth.Hash(token),
		CreatedAt: time.Now().UTC(),
	}}}
	require.NoError(t, store.Save(path))

	return path, token
}

// statusOf issues a GET with an optional Authorization header and returns the
// status code.
func statusOf(t *testing.T, url, authHeader string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)

	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
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

func TestIsTokenCommand_ShouldRecogniseOnlyTheTokenVerb(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "should route token management to the CLI", args: []string{"token", "list"}, want: true},
		{name: "should run the server when no args are given", args: nil, want: false},
		{name: "should run the server for flags", args: []string{"--port", "8444"}, want: false},
		{name: "should not match a flag that merely contains the verb", args: []string{"--token-file", "x"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tt.want, isTokenCommand(tt.args))
		})
	}
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldServeHealthUntilContextCancelled(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	port := freePort(t)
	tokensPath, token := tokenStore(t)
	errCh := make(chan error, 1)

	go func() {
		errCh <- run(ctx, withIdentity("--port", strconv.Itoa(port), "--auth-tokens-file", tokensPath))
	}()

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

	// ASSERT — the config port, health handler, and middleware chain are wired,
	// and health answers without a credential even though auth is enabled.
	//
	// 503, not 200, and that is the correct answer rather than a broken test.
	// The adapter now probes a real DHCP backend, and this host has no
	// powershell.exe — so the dhcp-server component is genuinely unavailable and
	// the overall status follows it. Asserting 200 would require the probe to
	// report a backend it cannot reach as working, which is the exact lie the
	// live probe exists to prevent.
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var body health.Response
	require.NoError(t, json.Unmarshal(raw, &body))

	assert.Equal(t, health.StatusUnavailable, body.Status)
	assert.Equal(t, version, body.Version)

	// The per-component split is what makes that answer useful: the adapter
	// itself is up and serving — only its backend is unreachable.
	components := make(map[string]health.Status, len(body.Components))
	for _, c := range body.Components {
		components[c.Name] = c.Status
	}

	assert.Equal(t, health.StatusHealthy, components["core"])
	assert.Equal(t, health.StatusUnavailable, components["dhcp-server"],
		"the probe must be wired into the binary, not just constructible")
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))

	// A protected path rejects an anonymous caller and accepts the token.
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/leases"
	assert.Equal(t, http.StatusUnauthorized, statusOf(t, base, ""))
	assert.Equal(t, http.StatusNotFound, statusOf(t, base, "Bearer "+token),
		"an authenticated caller should get past auth and reach the mux")

	// Cancellation stands in for Ctrl+C, which main translates from a signal.
	cancel()
	require.NoError(t, <-errCh)

	rec.AssertEmitted(t, catalog.SYS001)
	rec.AssertData(t, catalog.SYS001, "version", version)
	rec.AssertEmitted(t, catalog.SYS004)
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldRefuseToStartWithoutTokens(t *testing.T) {
	// ARRANGE — each case is a store the operator believes is serviceable and
	// that would in fact 401 every request.
	expired := filepath.Join(t.TempDir(), "expired.toml")
	expiredStore := &auth.Store{Tokens: []auth.Entry{{
		Label:     "long-gone",
		Hash:      auth.Hash("wadapt_whatever"),
		CreatedAt: time.Now().UTC().AddDate(0, 0, -91),
		ExpiresAt: auth.NewExpiry(time.Now().UTC().AddDate(0, 0, -1)),
	}}}
	require.NoError(t, expiredStore.Save(expired))

	empty := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, (&auth.Store{}).Save(empty))

	tests := []struct {
		name       string
		tokensFile string
		wantErr    string
	}{
		{
			name:       "should fail when the token file does not exist",
			tokensFile: filepath.Join(t.TempDir(), "absent.toml"),
			wantErr:    "token gen --label",
		},
		{
			name:       "should fail when the store holds no tokens",
			tokensFile: empty,
			wantErr:    "no tokens configured",
		},
		{
			name:       "should fail when every token in the store has expired",
			tokensFile: expired,
			wantErr:    "have expired",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // observability.Setup is global
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			// ACT
			err := run(t.Context(), withIdentity(
				"--port", strconv.Itoa(freePort(t)),
				"--auth-tokens-file", tt.tokensFile,
			))

			// ASSERT — the message has to tell an operator what to do, and the
			// server must never reach the point of announcing itself.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			rec.AssertNotEmitted(t, catalog.SYS002)
		})
	}
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldWarnLoudlyWhenAuthIsDisabled(t *testing.T) {
	// ARRANGE — no token file at all, which is only survivable with disableAuth.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	port := strconv.Itoa(freePort(t))

	go func() {
		errCh <- run(ctx, withIdentity("--port", port, "--disable-auth"))
	}()

	waitForListening(t, rec)

	// ACT
	cancel()

	// ASSERT — a wide-open server is exactly the state an operator must be able
	// to find in the log afterwards.
	require.NoError(t, <-errCh)
	rec.AssertEmitted(t, catalog.SYS006)
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRunServer_ShouldTreatHelpAsSuccess(t *testing.T) {
	// ARRANGE — the FlagSet writes usage to stderr; only the outcome matters here.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	err := runServer([]string{"--help"})

	// ASSERT — asking for help is not a failed startup. `token gen --help`
	// already exits 0 out of this same binary, and the two must agree.
	require.NoError(t, err)
	rec.AssertNotEmitted(t, catalog.SYS005)
}

//nolint:paralleltest // observability.Setup replaces the global slog logger
func TestRun_ShouldReturnErrorWhenConfigInvalid(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT — out of range, so Load fails validation.
	err := run(t.Context(), withIdentity("--port", "70000"))

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

	tokensPath, _ := tokenStore(t)

	// ACT
	err = run(t.Context(), withIdentity("--port", strconv.Itoa(port), "--auth-tokens-file", tokensPath))

	// ASSERT — a bind conflict is a startup error, not a silent no-op.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listening on")
}
