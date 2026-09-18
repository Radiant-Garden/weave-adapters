//go:build servicegate && windows

/*
Testing: shared harness for the service lifecycle gate (no corresponding .go
file)

Pending:

Tested:

	Nothing directly — this is the plumbing servicegate_test.go drives:
	running the adapter's own subcommands, shelling to PowerShell and sc.exe,
	reading the Event Log, reading ACLs through Get-Acl rather than through
	our own reader, and dumping evidence when a step fails.

Tested elsewhere:

	What it drives: servicegate_test.go, the twenty-three steps.

Declined:

	Tests for the helpers. They have no behaviour their caller does not
	exercise on every run, and this whole file only executes on an elevated
	WS2022 host — a test for it would need the same host to say less.

Additional Remarks:

	ACLs are read back with Get-Acl, deliberately, and never with
	winsvc.ReadGrants. Our own reader verifying our own writer is not
	evidence: if the ACE walk misreads a descriptor, a gate using it would
	agree with the bug. Get-Acl is a different implementation and the tool an
	operator would reach for.

	THIS GATE MUTATES THE HOST. It registers, starts, kills and removes a
	Windows service, rewrites ACLs on its own scratch directory, and runs the
	e2e write suite against the real DHCP server. It is elevated-manual for
	that reason and is not in any CI gate.
*/
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
)

// gateBinaryEnv names the binary the gate drives, and therefore the path the
// SCM records for every service it registers.
//
// Required, and it must be durable. `service install` registers
// os.Executable(), so installing from a build under t.TempDir() would point
// the host's REAL service at a path that disappears when this test ends: it
// keeps running until the next restart and then fails with a file-not-found
// nobody can trace back to here. The Taskfile builds bin\ and passes it.
const gateBinaryEnv = "WADAPT_GATE_BINARY"

// provisionedConfigEnv names the production config the gate reinstalls
// against at the end. Required: the gate's whole invariant is that the host
// is left with the real service running, and it cannot honour that if nobody
// told it what "real" is.
const provisionedConfigEnv = "WADAPT_PROVISIONED_CONFIG"

// gate is one run: the paths it owns and the binary it drives.
type gate struct {
	binary string
	// dir is the gate's own scratch directory. Everything it writes lives
	// here, including the config it installs against — never the provisioned
	// one, which step 21 restores.
	dir        string
	configPath string
	tokenStore string
	logFile    string
	token      string
	port       int
	baseURL    string

	// drainWindowStart bounds the log search in step 15, so it observes the
	// drain that step 14 provoked rather than an earlier clean shutdown.
	drainWindowStart time.Time

	// Part D's own layout. A SEPARATE directory from the one above, because
	// setup writes the config itself and the whole point of that part is to
	// exercise the writing -- pointing it at a config the gate hand-wrote
	// would take the never-rewrite branch and prove nothing.
	setupDir  string
	setupPort int
	keyFile   string
}

// The identity Part D provisions with.
//
// FIXED, and the same key the hand-written config above carries. Part D
// asserts that a config setup RENDERED derives the same wadaptIDs as one
// written by hand from the same inputs, and a generated key would make that
// assertion agree with whatever had just been invented.
const (
	gateNamespaceKey = "service-gate-namespace-key-0123456789"
	gateServerName   = "dhcp01.gate.test"
)

// newGate builds the binary and lays out a configuration the gate owns.
func newGate(t *testing.T) *gate {
	t.Helper()

	requireElevated(t)

	// Not t.TempDir(), which the framework deletes on the way out: when a
	// step fails, the evidence dump tells whoever is on call that the scratch
	// directory was KEPT, and a directory the harness owns is what makes that
	// true.
	//nolint:usetesting // the directory must survive a failed run; see above.
	dir, err := os.MkdirTemp("", "wadapt-gate-")
	require.NoError(t, err)

	g := &gate{
		binary:     durableBinary(t),
		dir:        dir,
		configPath: filepath.Join(dir, "config.toml"),
		tokenStore: filepath.Join(dir, "tokens.toml"),
		// A FRESH path for this run. Every log assertion below is then about
		// this run and cannot pass on a stale file left by the last one.
		logFile: filepath.Join(dir, fmt.Sprintf("adapter-%d.log", time.Now().UnixNano())),
		port:    freePort(t),
	}

	g.baseURL = fmt.Sprintf("http://127.0.0.1:%d", g.port)

	// Part D's layout, laid out here so a failed run leaves it beside
	// everything else the evidence dump points at.
	g.setupDir = filepath.Join(dir, "setup")
	g.keyFile = filepath.Join(dir, "ns.key")
	g.setupPort = freePort(t)

	t.Cleanup(func() { g.dumpEvidenceOnFailure(t) })

	return g
}

// durableBinary returns the binary the gate drives, failing if it is absent.
//
// Never buildAdapter: that builds into t.TempDir(), and what the SCM records
// has to outlive the test that registered it.
func durableBinary(t *testing.T) string {
	t.Helper()

	path := os.Getenv(gateBinaryEnv)
	require.NotEmpty(t, path, "%s must name a durable binary: `service install` registers the running "+
		"executable's path, so a temporary build would leave the host's service pointing at nothing",
		gateBinaryEnv)

	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	require.FileExists(t, abs, "%s names %s, which does not exist; run `task build-windows` first",
		gateBinaryEnv, abs)

	return abs
}

// writeConfig writes the gate's config file, with extra appended verbatim.
func (g *gate) writeConfig(t *testing.T, extra string) {
	t.Helper()

	body := fmt.Sprintf(`port = %d
logFile = '%s'
authTokensFile = '%s'

[identity]
# FIXED, not random. The e2e restart test compares wadaptIDs across a real
# service restart, and randomising these would make that pass vacuously.
namespaceKey = 'service-gate-namespace-key-0123456789'
serverName = 'dhcp01.gate.test'
%s`, g.port, g.logFile, g.tokenStore, extra)

	require.NoError(t, os.WriteFile(g.configPath, []byte(body), 0o600))
}

// adapterCmd runs one of the adapter's own subcommands.
func (g *gate) adapterCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	//nolint:gosec,noctx // G204: the binary this gate just built, with constant verbs.
	cmd := exec.Command(g.binary, args...)

	out, err := cmd.CombinedOutput()
	t.Logf("$ %s %s\n%s", filepath.Base(g.binary), strings.Join(args, " "), out)

	return string(out), err
}

// mustAdapter runs a subcommand and fails the test if it does not succeed.
func (g *gate) mustAdapter(t *testing.T, args ...string) string {
	t.Helper()

	out, err := g.adapterCmd(t, args...)
	require.NoError(t, err, "%s %s", filepath.Base(g.binary), strings.Join(args, " "))

	return out
}

// install registers the service against the gate's own config.
func (g *gate) install(t *testing.T) {
	t.Helper()

	g.mustAdapter(t, "service", "install", "--config", g.configPath, "--"+consentFlag)
}

// ps runs a PowerShell script and returns its output.
//
// The script must RUN TO COMPLETION and leave nothing behind. A detached
// launch -- Start-Process, Start-Job, `&` -- produces a grandchild that
// outlives the test binary holding the stream `go test` reads its output
// through, and the run then ends in `Test I/O incomplete 1m0s after exiting`
// with the actual verdict discarded. Whatever wanted detaching almost
// certainly wanted Go's own net/http instead; see the warm-up in
// servicegate_reboot_test.go.
func ps(t *testing.T, script string) string {
	t.Helper()

	out, err := psErr(t, script)
	require.NoError(t, err, "powershell: %s\n%s", script, out)

	return out
}

// psErr runs a PowerShell script and returns its output and error, for the
// steps that expect a failure.
func psErr(t *testing.T, script string) (string, error) {
	t.Helper()

	//nolint:gosec,noctx // G204: gate-authored scripts on a host this gate already administers.
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)

	out, err := cmd.CombinedOutput()
	t.Logf("PS> %s\n%s", script, out)

	return strings.TrimSpace(string(out)), err
}

// sc runs sc.exe and returns its output and error.
func sc(t *testing.T, args ...string) (string, error) {
	t.Helper()

	//nolint:gosec,noctx // G204: constant verbs against a constant service name.
	cmd := exec.Command("sc.exe", args...)

	out, err := cmd.CombinedOutput()
	t.Logf("$ sc.exe %s\n%s", strings.Join(args, " "), out)

	return string(out), err
}

// requireElevated fails early rather than letting the first SCM call fail
// obscurely twenty steps in.
func requireElevated(t *testing.T) {
	t.Helper()

	out, err := psErr(t, `([Security.Principal.WindowsPrincipal]::new(`+
		`[Security.Principal.WindowsIdentity]::GetCurrent())).IsInRole(`+
		`[Security.Principal.WindowsBuiltInRole]::Administrator)`)
	require.NoError(t, err)
	require.Equal(t, "True", strings.TrimSpace(out),
		"this gate installs a Windows service and rewrites ACLs; run it from an elevated prompt")
}

// aclEntry is one access rule as Get-Acl reports it.
type aclEntry struct {
	// SID, not a name: see acl() for why a localized name cannot survive the
	// trip out of PowerShell.
	SID              string
	FileSystemRights string
	IsInherited      bool
	InheritanceFlags string
}

// acl reads a path's access rules through Get-Acl.
//
// A different implementation from winsvc.ReadGrants on purpose: our own
// reader confirming our own writer would agree with a bug in either.
func acl(t *testing.T, path string) (entries []aclEntry, protected bool, owner string) {
	t.Helper()

	// The SID is resolved INSIDE PowerShell, and the name is never returned.
	//
	// Round-tripping the name does not survive the trip. Get-Acl renders it in
	// the host's language -- "NT-AUTORITÄT\SYSTEM" on this German server --
	// the console emits it in the OEM codepage, Go reads those bytes as UTF-8,
	// and the "Ä" is destroyed. Feeding the result back to .Translate() then
	// fails with IdentityNotMapped. The policy has always been written in SIDs
	// precisely because names are locale-dependent; this keeps the gate to the
	// same rule instead of translating at the one boundary that mangles them.
	raw := ps(t, fmt.Sprintf(`$a = Get-Acl -LiteralPath '%s'
[pscustomobject]@{
  Owner     = $a.Owner
  Protected = $a.AreAccessRulesProtected
  Rules     = @($a.Access | ForEach-Object { [pscustomobject]@{
      SID              = $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
      FileSystemRights = $_.FileSystemRights.ToString()
      IsInherited      = $_.IsInherited
      InheritanceFlags = $_.InheritanceFlags.ToString()
  }})
} | ConvertTo-Json -Depth 4 -Compress`, path))

	var parsed struct {
		Owner     string     `json:"Owner"`
		Protected bool       `json:"Protected"`
		Rules     []aclEntry `json:"Rules"`
	}

	require.NoError(t, json.Unmarshal([]byte(raw), &parsed), "Get-Acl output: %s", raw)

	return parsed.Rules, parsed.Protected, parsed.Owner
}

// eventLog returns the most recent entries for the adapter's source.
func eventLog(t *testing.T, entries int) string {
	t.Helper()

	out, _ := psErr(t, fmt.Sprintf(
		`Get-WinEvent -ProviderName %s -MaxEvents %d -ErrorAction SilentlyContinue | `+
			`Select-Object TimeCreated, Id, Message | Format-List | Out-String -Width 200`,
		serviceName, entries))

	return out
}

// logContains reports whether the gate's log file holds needle.
func (g *gate) logContains(t *testing.T, needle string) bool {
	t.Helper()

	body, err := os.ReadFile(g.logFile)
	if err != nil {
		return false
	}

	return strings.Contains(string(body), needle)
}

// logBetween returns the gate's log lines whose timestamps fall inside the
// window.
//
// A whole-file scan is the wrong tool once the gate has stopped and started
// the service several times: SYS-004 appears after every clean stop, so an
// unbounded search finds one and says nothing about the stop under test.
func (g *gate) logBetween(t *testing.T, from, to time.Time) string {
	t.Helper()

	body, err := os.ReadFile(g.logFile)
	require.NoError(t, err, "reading %s", g.logFile)

	var out strings.Builder

	for line := range strings.SplitSeq(string(body), "\n") {
		stamp, ok := logLineTime(line)
		if !ok || stamp.Before(from) || stamp.After(to) {
			continue
		}

		out.WriteString(line)
		out.WriteString("\n")
	}

	return out.String()
}

// state returns the SCM's current state word for the service, or "ABSENT".
func state(t *testing.T) string {
	t.Helper()

	out, err := sc(t, "query", serviceName)
	if err != nil {
		return "ABSENT"
	}

	for _, word := range []string{"RUNNING", "STOPPED", "START_PENDING", "STOP_PENDING"} {
		if strings.Contains(out, word) {
			return word
		}
	}

	return "UNKNOWN"
}

// waitState polls until the service reports want, and fails with the state it
// actually reached.
func waitState(t *testing.T, want string, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := state(t); got == want {
			return
		}

		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("the service did not reach %s within %s; it is %s", want, within, state(t))
}

// dumpEvidenceOnFailure prints the three things every host-found defect in
// this milestone turned out to be diagnosable from, and nothing else was.
//
// Without them a failure reads as "step 13 failed" and sends whoever is on
// call back to the host over RDP to collect what the gate was already
// standing next to.
func (g *gate) dumpEvidenceOnFailure(t *testing.T) {
	t.Helper()

	if !t.Failed() {
		_ = os.RemoveAll(g.dir)

		return
	}

	t.Logf("=== EVIDENCE ===\nthe gate's scratch directory is kept at %s", g.dir)

	t.Logf("--- service status ---\n%s", func() string {
		out, _ := g.adapterCmd(t, "service", "status")

		return out
	}())

	t.Logf("--- last 30 Event Log entries ---\n%s", eventLog(t, 30))

	if body, err := os.ReadFile(g.logFile); err == nil {
		tail := string(body)
		if len(tail) > 8000 {
			tail = "…" + tail[len(tail)-8000:]
		}

		t.Logf("--- tail of %s ---\n%s", g.logFile, tail)
	} else {
		t.Logf("--- no log file at %s: %v ---", g.logFile, err)
	}
}

// registeredImagePath reads the command line the SCM recorded.
func registeredImagePath(t *testing.T) string {
	t.Helper()

	return ps(t, fmt.Sprintf(
		`(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\%s').ImagePath`, serviceName))
}

// servicePID returns the process ID the SCM has for the service, or zero.
func servicePID(t *testing.T) int {
	t.Helper()

	out, err := psErr(t, fmt.Sprintf(
		`(Get-CimInstance Win32_Service -Filter "Name='%s'").ProcessId`, serviceName))
	if err != nil {
		return 0
	}

	var pid int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(out), "%d", &pid); scanErr != nil {
		return 0
	}

	return pid
}

// countEventID returns how many entries the source has at a numeric event ID.
//
// Used to observe the SCM restarting a failing service: the count of SYS-005
// going up is the only externally visible evidence that the recovery schedule
// fired, short of watching the process list.
func countEventID(t *testing.T, id int) int {
	t.Helper()

	out, err := psErr(t, fmt.Sprintf(
		`@(Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='%s'; Id=%d} `+
			`-ErrorAction SilentlyContinue).Count`, serviceName, id))
	if err != nil {
		return 0
	}

	var n int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); scanErr != nil {
		return 0
	}

	return n
}

// slowStub is a PowerShell script that sleeps before answering, so a request
// is genuinely in flight when a stop arrives.
//
// An existing config knob (dhcp.powershellPath), not a fake compiled into
// production code: the adapter shells out, so a slow shell is a slow backend.
// EIGHT SECONDS, not twenty. It has to be long enough that a request is
// genuinely in flight when the stop lands, and SHORTER than the drain budget
// so the drain completes -- step 15 asserts SYS-004 and the absence of
// SYS-007, and a backend slower than the budget would produce exactly the
// truncation that step exists to rule out.
const slowStub = `param([Parameter(ValueFromRemainingArguments=$true)]$Rest)
Start-Sleep -Seconds 8
Write-Output '[]'
`

// startWithSlowBackend reconfigures the service to use the sleeping stub and
// starts it.
func (g *gate) startWithSlowBackend(t *testing.T) {
	t.Helper()

	stub := filepath.Join(g.dir, "slow-backend.ps1")
	require.NoError(t, os.WriteFile(stub, []byte(slowStub), 0o600))

	wrapper := filepath.Join(g.dir, "slow-backend.cmd")
	require.NoError(t, os.WriteFile(wrapper,
		[]byte(fmt.Sprintf("@powershell.exe -NoProfile -File \"%s\" %%*\r\n", stub)), 0o600))

	// A command timeout longer than the sleep, or the backend call is
	// cancelled before the drain has anything to wait for.
	// A command timeout comfortably past the sleep, and a probe timeout below
	// it, since the adapter requires probe < command.
	g.writeConfig(t, fmt.Sprintf(
		"\n[dhcp]\npowershellPath = '%s'\ncommandTimeout = '30s'\nprobeTimeout = '25s'\n", wrapper))

	// STOPPED FIRST, and not because the service might be running: because the
	// configuration just changed underneath it. A service already up is still
	// serving with the old backend, and `service start` is idempotent now --
	// it would report success and the slow stub would never be used.
	g.mustAdapter(t, "service", "stop")
	waitState(t, "STOPPED", time.Minute)

	g.mustAdapter(t, "service", "secure", "--config", g.configPath)
	g.mustAdapter(t, "service", "start")
	waitState(t, "RUNNING", time.Minute)
}

// beginSlowRequest fires a request that will still be running when the caller
// asks the service to stop, and returns a channel closed once it finishes.
//
// Detached from the test's own logging on purpose. The goroutine outlives the
// subtest that started it, and calling t.Logf after a test has returned
// panics -- which would surface as a mysterious failure in whichever step ran
// next rather than here.
func beginSlowRequest(g *gate) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		// The outcome does not matter: what matters is that the handler is
		// inside a backend call when the stop lands.
		//nolint:gosec,noctx // G204: a gate-authored script against its own loopback service.
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf(
				`try { Invoke-WebRequest -Uri '%s/api/v1/scopes' -Headers @{Authorization='Bearer %s'} `+
					`-TimeoutSec 120 -UseBasicParsing | Out-Null } catch { }`,
				g.baseURL, g.token))

		_ = cmd.Run()
	}()

	return done
}

// runE2EAgainst runs the e2e suite against the installed service.
//
// A separate `go test` invocation rather than calling the tests directly:
// they live behind the e2e build tag, and shelling out is what lets the same
// suite serve both gates unmodified. Attach mode is selected by environment,
// so nothing in e2e_test.go knows a service exists.
func runE2EAgainst(t *testing.T, g *gate) {
	t.Helper()

	// ".", not "./cmd/...". go test runs each package in its own directory, so
	// the working directory here is already cmd/weave-adapter-dhcp-windows --
	// and that is where the e2e tests live, in this very package. The repo-root
	// pattern resolves to nothing from here.
	//
	//nolint:noctx // G204 does not apply: every argument is a constant.
	cmd := exec.Command("go", "test", "-count=1", "-tags", "e2e", "-run", "TestE2E", "-v", ".")

	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%d", attachPortEnv, g.port),
		attachTokenEnv+"="+g.token,
	)

	out, err := cmd.CombinedOutput()
	t.Logf("--- e2e against the service ---\n%s", out)
	require.NoError(t, err, "the e2e suite failed against the installed service")
}

// resolveProvisioned loads a config file the way the server does, so the
// reboot halves read the same values the running service did rather than
// re-deriving them from a second source.
func resolveProvisioned(t *testing.T, configPath string) *config.Values {
	t.Helper()

	values, err := config.LoadWithoutEnvironment(
		append(config.CoreKeys(), dhcpwindows.Keys()...),
		[]string{"--config", configPath},
	)
	require.NoError(t, err, "reading the provisioned config %q", configPath)

	return values
}

// configuredLogFile returns the log the provisioned service writes to.
func configuredLogFile(t *testing.T, configPath string) string {
	t.Helper()

	logFile := resolveProvisioned(t, configPath).String(config.KeyLogFile)
	require.NotEmpty(t, logFile, "the provisioned config sets no logFile, so nothing survives the reboot")

	return logFile
}

// configuredBaseURL returns where the provisioned service serves.
func configuredBaseURL(t *testing.T, configPath string) string {
	t.Helper()

	return fmt.Sprintf("http://127.0.0.1:%d", resolveProvisioned(t, configPath).Int(config.KeyPort))
}

// lastBootTime returns when the host last started.
func lastBootTime(t *testing.T) time.Time {
	t.Helper()

	raw := ps(t, `(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToString("o")`)

	booted, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	require.NoError(t, err, "parsing the last boot time %q", raw)

	return booted
}

// lastEventBetween reports whether the log holds an entry for id with a
// timestamp inside the window.
//
// The window is what makes the reboot assertion mean anything: a SYS-004 from
// the service stopping again AFTER the reboot would satisfy a naive search
// while saying nothing about the shutdown in between.
func lastEventBetween(t *testing.T, logBody, id string, from, to time.Time) bool {
	t.Helper()

	for line := range strings.SplitSeq(logBody, "\n") {
		if !strings.Contains(line, id) {
			continue
		}

		stamp, ok := logLineTime(line)
		if !ok {
			continue
		}

		if stamp.After(from) && stamp.Before(to) {
			t.Logf("%s at %s, inside the window %s..%s", id,
				stamp.Format(time.RFC3339), from.Format(time.RFC3339), to.Format(time.RFC3339))

			return true
		}
	}

	return false
}

// logLineTime pulls the timestamp out of a text-handler log line, which opens
// with `time=<RFC3339>`.
func logLineTime(line string) (time.Time, bool) {
	const prefix = "time="

	_, rest0, found := strings.Cut(line, prefix)
	if !found {
		return time.Time{}, false
	}

	rest := rest0
	if before, _, found := strings.Cut(rest, " "); found {
		rest = before
	}

	stamp, err := time.Parse(time.RFC3339Nano, rest)

	return stamp, err == nil
}

// waitAbsent blocks until the SCM reports the service gone.
//
// Windows removes a registration only once the last handle closes, so an
// uninstall that returned is not yet an uninstall that took effect -- and
// installing over the gap fails with "already installed" for a service that is
// on its way out.
func waitAbsent(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) && state(t) != "ABSENT" {
		time.Sleep(time.Second)
	}

	require.Equal(t, "ABSENT", state(t), "the service is still registered")
}

// exitCode returns the process exit code behind an error from adapterCmd.
//
// setup's codes are its machine-readable contract while --output json is
// deferred, so the gate has to read them rather than treat every non-zero the
// same: 4 means installed, running and the backend unhealthy, which is a
// success on a host whose DHCP server is briefly out and a failure nowhere.
func exitCode(err error) int {
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode()
	}

	return -1
}
