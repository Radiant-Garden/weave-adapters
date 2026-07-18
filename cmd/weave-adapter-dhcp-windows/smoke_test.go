//go:build smoke

/*
Testing: the built binary, end to end

Pending:

Tested:
  the adapter as a shipped artifact
    - TestSmoke_ShouldServeHealthAndEnforceAuthWhenRunAsABinary: builds the
      binary, mints a token through its own CLI, runs it as a subprocess and
      drives it over a real socket: health answers 200 unauthenticated, a
      non-exempt route is 401 anonymous and 404 with the token, and the process
      exits 0 on an interrupt.

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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	healthPath  = "/api/v1/health"
	guardedPath = "/api/v1/does-not-exist"
)

// bearerLine matches the line the token CLI prints for pasting into weave. It
// is the unambiguous one to parse: the bare-token line above it is
// distinguished only by indentation.
var bearerLine = regexp.MustCompile(`(?m)^\s*Bearer\s+(\S+)\s*$`)

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
	require.Equal(t, http.StatusOK, status, "health must answer without credentials: %s", body)

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

	assert.Equal(t, "healthy", health.Status)
	assert.NotEmpty(t, health.Version, "version is empty -- check that -ldflags reached the build")
	assert.Contains(t, componentNames(health.Components), "core")

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

// buildAdapter compiles the package under test into a temporary directory and
// returns the path. Building here rather than reusing bin/ keeps the test
// self-contained: it cannot pass against a stale artifact from an earlier run.
func buildAdapter(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "adapter")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}

	output, err := exec.Command("go", "build", "-o", out, ".").CombinedOutput()
	require.NoError(t, err, "building the adapter: %s", output)

	return out
}

// mintToken drives the token CLI the way an operator does, so the store this
// test authenticates against is one the shipped binary wrote.
func mintToken(t *testing.T, binary, store string) string {
	t.Helper()

	output, err := exec.Command(binary, "token", "gen", "--label", "smoke", "--file", store).CombinedOutput()
	require.NoError(t, err, "minting a token: %s", output)

	match := bearerLine.FindSubmatch(output)
	require.NotNil(t, match, "no 'Bearer <token>' line in token gen output:\n%s", output)

	return string(match[1])
}

// startAdapter runs the binary with its output captured to a file, dumped only
// if the test fails, and guarantees the process is reaped either way.
func startAdapter(t *testing.T, binary string, port int, store string) *exec.Cmd {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), "adapter.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WEAVE_ADAPTER_PORT=%d", port),
		"WEAVE_ADAPTER_AUTH_TOKENS_FILE="+store,
		"WEAVE_ADAPTER_LOG_SEVERITY=debug",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	newProcessGroup(cmd)

	require.NoError(t, cmd.Start(), "starting the adapter")

	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}

		_ = logFile.Close()

		if t.Failed() {
			if out, readErr := os.ReadFile(logPath); readErr == nil {
				t.Logf("--- adapter output ---\n%s", out)
			}
		}
	})

	return cmd
}

// waitReady blocks until the adapter accepts connections, failing the test
// rather than hanging if it never does.
func waitReady(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		//nolint:noctx // a fixed-timeout client is the point; there is no caller context here.
		resp, err := (&http.Client{Timeout: time.Second}).Get(url)
		if err == nil {
			_ = resp.Body.Close()

			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("adapter did not accept connections on %s within 15s", url)
}

// get issues a request, optionally bearing a token, and returns the status and
// body together so assertions can report both.
func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, body
}

// waitFor reaps the process on a channel so the caller can bound the wait.
func waitFor(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	return done
}

func componentNames(components []struct {
	Name   string `json:"name"`
	Status string `json:"status"`
},
) []string {
	names := make([]string, 0, len(components))
	for _, c := range components {
		names = append(names, c.Name)
	}

	return names
}
