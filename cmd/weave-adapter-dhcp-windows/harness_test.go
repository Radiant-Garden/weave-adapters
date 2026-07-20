//go:build smoke || e2e

/*
Testing: shared harness for the gates that drive the built binary (no
corresponding .go file)

Pending:

Tested:

	Nothing directly — this file is the plumbing the smoke and e2e gates share:
	building the artifact, minting a token through the real CLI, starting the
	process, waiting for the socket, and reaping it.

Tested elsewhere:

	What the helpers drive: smoke_test.go (the artifact starts, serves and shuts
	down) and e2e_test.go (the full read path against a real DHCP backend).

Declined:

	Tests for the helpers themselves. They have no behaviour a test could pin
	that their two callers do not already exercise on every run — a broken
	buildAdapter fails both gates immediately and by name.

Additional Remarks:

	Tagged `smoke || e2e` so one copy serves both. The alternative was a second
	copy behind the e2e tag, and a harness that drifts between two gates is worse
	than no harness: they would stop proving the same artifact starts the same
	way.
*/
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// bearerLine matches the line the token CLI prints for pasting into weave. It
// is the unambiguous one to parse: the bare-token line above it is
// distinguished only by indentation.
var bearerLine = regexp.MustCompile(`(?m)^\s*Bearer\s+(\S+)\s*$`)

// buildAdapter compiles the package under test into a temporary directory and
// returns the path. Building here rather than reusing bin/ keeps the test
// self-contained: it cannot pass against a stale artifact from an earlier run.
func buildAdapter(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "adapter")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}

	// G204/noctx: compiling the package under test is this helper's entire
	// purpose, and every argument is a constant or a t.TempDir() path.
	//nolint:gosec,noctx // G204: constant args; the output path is test-owned.
	output, err := exec.Command("go", "build", "-o", out, ".").CombinedOutput()
	require.NoError(t, err, "building the adapter: %s", output)

	return out
}

// mintToken drives the token CLI the way an operator does, so the store this
// test authenticates against is one the shipped binary wrote.
func mintToken(t *testing.T, binary, store string) string {
	t.Helper()

	//nolint:gosec,noctx // G204: binary is the artifact we just built; store is test-owned.
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
	//nolint:gosec // G304: logPath is inside this test's own TempDir.
	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	// The process is reaped by the Cleanup below rather than by a context: the
	// test drives its shutdown deliberately, since signal delivery is part of
	// what it checks.
	//nolint:noctx // the Cleanup below reaps it; see above.
	cmd := exec.Command(binary)

	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WEAVE_ADAPTER_PORT=%d", port),
		"WEAVE_ADAPTER_AUTH_TOKENS_FILE="+store,
		"WEAVE_ADAPTER_LOG_SEVERITY=debug",
		// KEEP THESE FIXED. The e2e restart test compares wadaptIDs across two
		// processes, and every ID derives from these two values — randomising
		// them per run would make that assertion pass vacuously while proving
		// nothing about derivation stability.
		//
		// Provisioned, not defaulted: the adapter refuses to start without
		// either, because a namespace key that regenerates on reinstall and a
		// server name that follows the hostname are both fleet-wide re-keys.
		// Setting them here is also what proves the binary reads them from the
		// environment, which is the provisioning path for a backup-critical value.
		"WEAVE_ADAPTER_IDENTITY_NAMESPACE_KEY=smoke-namespace-key-0123456789",
		"WEAVE_ADAPTER_IDENTITY_SERVER_NAME=dhcp01.smoke.test",
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
			//nolint:gosec // G304: logPath is inside this test's own TempDir.
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

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
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

// post sends a JSON body and returns the status, body and response headers.
//
// Headers are returned because Location is the point of the create test: a 201
// that does not say where the resource lives is a contract failure the body
// cannot reveal.
func post(t *testing.T, url, token, body string) (int, []byte, http.Header) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, payload, resp.Header
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
