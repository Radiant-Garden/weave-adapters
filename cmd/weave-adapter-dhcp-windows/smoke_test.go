//go:build smoke

/*
Testing: the built binary, end to end

Pending:
  Exercising the *green* path end to end. Nothing in CI can reach a real DHCP
    server, so the healthy branch is covered only against a fake runner in the
    adapter's own probe tests. Pointing --dhcp-powershell-path at a stub that
    replays a captured fixture would close this using an existing config knob
    rather than a fake in production code; it needs a cross-platform stub,
    since this gate also runs on Windows.

Tested:
  the adapter as a shipped artifact
    - TestSmoke_ShouldServeHealthAndEnforceAuthWhenRunAsABinary: builds the
      binary, mints a token through its own CLI, runs it as a subprocess and
      drives it over a real socket: health answers unauthenticated, a non-exempt
      route is 401 anonymous and 404 with the token, /openapi.yaml serves the
      embedded contract verbatim without a credential, and the process exits 0
      on an interrupt. The backend component's verdict is deliberately not pinned:
      it depends on whether the host running this gate has a reachable DHCP
      server, and the WS2022 sign-off host does.

Tested elsewhere:
  freePort: declared in main_test.go, which compiles alongside this file since
    it carries no build tag. Reused rather than redeclared.
  run(ctx, args): main_test.go drives the same startup path in-process, which
    is faster and covers the error branches. This file exists for what that
    cannot reach -- the compiled artifact, the token CLI as a user invokes it,
    and OS-level signal delivery to a separate process.
  the health payload's shape: health package tests.
  auth rejection reasons: auth package tests.

Declined:
  Asserting on the log lines the adapter emits. The event catalog is tested at
  its source, and matching log text here would fail on rewording rather than on
  behaviour.
  Running under -race. The gate that runs this on Windows has no C toolchain,
  so the tag would silently do nothing there; ubuntu covers the detector.

Additional Remarks:
  Behind a build tag because it compiles the binary and binds a port -- too slow
  and too stateful for `go test ./...`. Run it with `task smoke`.

  This replaces scripts/smoke-windows.ps1. That script only ran on Windows, so
  Linux never exercised any of it, and every failure it produced was a
  PowerShell defect rather than an adapter one. In Go the same assertions run on
  every platform from one source, which means a regression surfaces on the fast
  ubuntu job instead of on a single serialized Windows host.

  The port is chosen at run time rather than fixed. The Windows runner is
  persistent, so a fixed port would collide with anything a cancelled earlier
  run left holding it.
*/

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apispec "github.com/radiantgarden/weave-adapters/api/dhcp-windows"
)

const (
	healthPath  = "/api/v1/health"
	guardedPath = "/api/v1/does-not-exist"
	openAPIPath = "/openapi.yaml"
)

// there is one test here, so parallelism would buy nothing and complicate reaping.
//
//nolint:paralleltest // builds the binary, binds a port and drives process shutdown;
func TestSmoke_ShouldServeHealthAndEnforceAuthWhenRunAsABinary(t *testing.T) {
	// ARRANGE
	binary := buildAdapter(t)
	store := filepath.Join(t.TempDir(), "tokens.toml")
	token := mintToken(t, binary, store)
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	adapter := startAdapter(t, binary, port, store)
	waitReady(t, base+healthPath)

	// ACT / ASSERT
	// Health is open by contract: weave polls it to decide whether the adapter
	// is reachable at all, so an auth failure there would read as an outage.
	// See httpserver.Unauthenticated. This is also M1's sign-off criterion.
	status, body := get(t, base+healthPath, "")

	// The status code is whatever this host's backend justifies, so it is
	// checked against the body below rather than pinned here. What matters at
	// this point is that health answered at all, and without a credential.
	require.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, status,
		"health must answer without credentials: %s", body)

	var health struct {
		Status        string `json:"status"`
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptimeSeconds"`
		Components    []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(body, &health), "health payload: %s", body)

	assert.NotEmpty(t, health.Version, "version is empty -- check that -ldflags reached the build")

	// Both components must be present, or the probe is not wired into the
	// shipped binary. Their verdicts are deliberately not pinned: this runner
	// has no powershell.exe and reports unavailable, while the WS2022 sign-off
	// host has the DHCP role and reports healthy. Asserting either would make
	// this gate test the environment rather than the artifact, and would fail
	// on the one host the milestone exists to validate against.
	assert.Contains(t, componentNames(health.Components), "core")
	assert.Contains(t, componentNames(health.Components), "dhcp-server")

	// Environment-independent, and the adapter's own rule: only unavailable
	// withholds a 200, because only unavailable means "stop routing here".
	if health.Status == "unavailable" {
		assert.Equal(t, http.StatusServiceUnavailable, status, "unavailable must be served as 503")
	} else {
		assert.Equal(t, http.StatusOK, status, "a serving adapter must answer 200")
	}

	// Health being open says nothing about whether auth is wired, so drive a
	// route that is not exempt. An unmatched path is the honest choice:
	// httpserver.Unauthenticated documents that paths matching no route still
	// authenticate, so an anonymous caller cannot enumerate what exists.
	//
	// The pair matters more than either half. 401 anonymous proves the
	// middleware is engaged; 404 with the token proves the token authenticated
	// and reached the router, rather than the 401 coming from somewhere else.
	status, body = get(t, base+guardedPath, "")
	assert.Equal(t, http.StatusUnauthorized, status, "anonymous caller must be rejected: %s", body)

	status, body = get(t, base+guardedPath, token)
	assert.Equal(t, http.StatusNotFound, status, "a valid token must reach the router: %s", body)

	// The served contract, which only this gate can prove reaches the wire. The
	// httpserver tests pass stand-in bytes and never see the real document, and
	// api/dhcp-windows checks the embed with no server involved -- so main.go
	// dropping WithOpenAPISpec, or the go:generate directive drifting from its
	// var, would leave every other test green while the shipped binary answered
	// 404 here.
	status, body = get(t, base+openAPIPath, "")
	require.Equal(t, http.StatusOK, status, "the spec must be served, unauthenticated: %s", body)
	assert.Equal(t, apispec.Spec(), body, "the binary must serve the embedded document verbatim")

	// The adapter is a console exe on Windows Server 2022, where main.go
	// assumes os.Interrupt arrives. Nothing verified that until this test.
	require.NoError(t, interrupt(adapter), "delivering an interrupt")

	select {
	case err := <-waitFor(adapter):
		require.NoError(t, err, "adapter must exit 0 after an interrupt")
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not exit within 10s of an interrupt -- graceful shutdown is hung")
	}
}
