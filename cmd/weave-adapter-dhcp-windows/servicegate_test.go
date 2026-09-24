//go:build servicegate && windows

/*
Testing: the adapter running as a real Windows service (no corresponding .go
file)

Pending:

	Part D, the reboot halves. They live in this file as two separate tests a
	human runs either side of a restart, because nothing in a test binary can
	survive one.

Tested:

	the lifecycle, which no other gate can observe
	  - install registers a recoverable service with the right ImagePath,
	    pre-shutdown deadline and failure-actions flag;
	  - the e2e suite passes against the SERVICE rather than a process a test
	    started;
	  - SYS-001 with runMode=service reaches both the log file and the Event
	    Log.

	the failure paths, which are the half that justifies this gate
	  - a bad config fails FAST and says why, rather than timing out;
	  - the SCM retries a clean non-zero exit, and an unclean death;
	  - it does NOT retry a clean stop;
	  - a stop issued mid-drain waits instead of erroring;
	  - the drain completes rather than being truncated;
	  - a widened token store refuses the start;
	  - a securable somebody else OWNS refuses the start, and `service secure`
	    takes ownership back;
	  - FILE_DELETE_CHILD alone on the data directory refuses the start.

	provisioning through `setup`, which is the command an operator actually
	runs
	  - it provisions a host from nothing: directory, config, token, install,
	    start and verify, all applied;
	  - the identity SURVIVES being provisioned from nothing -- a rendered
	    config derives the same wadaptIDs as a hand-written one;
	  - everything it created is locked down and PROTECTED, not merely
	    inheriting;
	  - a re-run reports every step satisfied, mints no second token and does
	    not rewrite the configuration;
	  - a conflicting server name is refused rather than applied.

	the three checks above are the ones no unit test can reach, and they are
	why this gate exists at all: each needs a real Windows security descriptor
	manipulated into a hostile shape, and off Windows ReadSecurity answers
	ErrUnsupported so every target is skipped. 18a is the sharpest -- the owner
	it sets is the INTERACTIVE ADMINISTRATOR, which is the owner Windows' own
	default policy ("object creator") gives a directory an operator made by
	hand, so it is the realistic case rather than a contrived one.

	the lockdown, read back through Get-Acl rather than our own reader
	  - the scratch directory before anything is registered, so a broken
	    applier fails with no service to tear down;
	  - SYSTEM and Administrators only, inheritance off, owner Administrators;
	  - a log file the SERVICE created inherited it;
	  - `service secure` is idempotent.

Tested elsewhere:

	Everything that does not need an SCM: `task ci` covers the controller's
	sequencing, the Event Log allocation, the path rules and every refusal in
	the subcommand. `task e2e` covers the server against a real DHCP backend.
	This file is for what only a registered service can answer.

Declined:

	Provoking the unreadable-descriptor refusal. It was written, run, and
	removed, and what it found is worth more than the test: on a correctly
	owned securable the condition is unreachable. The owner always holds
	READ_CONTROL implicitly whatever the access list says -- the property
	checkOwner's own comment rests on -- `service secure` sets the owner to
	Administrators, and the LocalSystem token carries that group. So the
	service always owns what it reads. Denying READ_CONTROL to SYSTEM does
	nothing (the read goes through the Administrators ACE) and denying it to
	Everyone does nothing either (the owner's implicit grant survives). Making
	a descriptor genuinely unreadable means first moving ownership away, and
	18a already refuses that. The branch is defence-in-depth behind the owner
	check, and internal/core/setup's unit tests are where it is exercised.

	Running in CI. Installing a service needs Administrator and the WS2022
	runner executes as NT AUTHORITY\\NetworkService. Weakening that account to
	make this CI-testable would hand a CI job the right to install services.

	Running concurrently with `task e2e`. Both reserve 198.51.100.0/24.

Additional Remarks:

	THIS GATE MUTATES THE HOST, and deliberately: it registers, starts, kills
	and removes a service, rewrites ACLs, and runs the e2e write suite against
	the real DHCP server.

	THE STEPS ARE ORDERED AND STATEFUL. They are subtests of one function
	precisely so they run in sequence; -shuffle does not reach inside a test.
	A failure stops the run, and the Taskfile's `defer:` still reinstalls --
	because a `go test` timeout kills this process without running any
	Cleanup, and a gate whose failure mode is "the host has no adapter" is
	worse than no gate.
*/
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/config"
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// The principals the ACL steps manipulate, as SIDs and not names: this host
// may not be English, and Get-Acl renders a name in the host's language.
//
// The leading * is icacls' own marker for "this is a SID, not an account
// name". winsvc.SIDAdministrators and SIDLocalSystem carry the bare form,
// which is what Get-Acl returns and what the assertions compare against.
const (
	// usersSID widens a target on purpose, in steps 18 and 18c.
	usersSID = "*S-1-5-32-545"
	// administratorsSID is the owner `service secure` must restore, in 18a.
	administratorsSID = winsvc.SIDAdministrators
)

//nolint:paralleltest,funlen // ordered, stateful, and one host; the steps are the test.
func TestServiceGate_ShouldInstallServeRecoverAndRemove(t *testing.T) {
	provisioned := os.Getenv(provisionedConfigEnv)
	require.NotEmpty(t, provisioned,
		"%s must name the production config: this gate leaves the host running the REAL service, "+
			"and cannot honour that without being told which one that is", provisionedConfigEnv)

	g := newGate(t)
	g.writeConfig(t, "")

	// --- Part A: the lifecycle -------------------------------------------

	t.Run("1: pre-clean by exact name", func(t *testing.T) {
		// Never a wadapt* sweep. A gate that deletes services by wildcard is
		// one typo away from removing something nobody asked it to.
		_, _ = g.adapterCmd(t, "service", "stop")
		_, _ = g.adapterCmd(t, "service", "uninstall", "--yes")

		require.Equal(t, "ABSENT", state(t), "the service is still registered after a pre-clean")
	})

	t.Run("2: an absent service is named, not guessed at", func(t *testing.T) {
		for _, verb := range []string{"start", "stop"} {
			out, err := g.adapterCmd(t, "service", verb)
			require.Error(t, err, "service %s against an absent service should fail", verb)
			assert.Contains(t, out+err.Error(), "not installed")
			assert.Contains(t, out+err.Error(), serviceName)
		}
	})

	t.Run("3: mint the gate's own token", func(t *testing.T) {
		// Into the GATE's store, never the provisioned one. The production
		// store holds hashes, so no token can be recovered from it -- and the
		// answer is to own the config outright rather than to leave a
		// credential behind in somebody else's.
		g.token = mintToken(t, g.binary, g.tokenStore)
		require.NotEmpty(t, g.token)
	})

	t.Run("4: the lockdown works before anything is registered", func(t *testing.T) {
		// SetNamedSecurityInfo, the ACE walk and the inline SID read have no
		// other coverage anywhere. Running them here means a broken applier
		// fails with NO SERVICE to tear down, and the later steps exercise
		// production paths rather than this code's first execution.
		g.mustAdapter(t, "service", "secure", "--config", g.configPath)

		rules, protected, owner := acl(t, g.configPath)
		assertLockedDown(t, g.configPath, rules, protected, owner)
	})

	t.Run("5: install registers a recoverable service", func(t *testing.T) {
		g.install(t)

		out := g.mustAdapter(t, "service", "status")

		// The flag whose absence makes every recovery assertion below pass
		// vacuously.
		assert.Contains(t, out, "restarts on failure",
			"the failure-actions flag is not set; step 13 would prove nothing")
		assert.NotContains(t, out, "WILL NOT restart")

		// The deadline, not the bare drain budget: the pre-shutdown timeout
		// is a hard wall, so it must cover the drain plus the moment the
		// controller needs to report Stopped afterwards.
		assert.Contains(t, out, winsvc.DrainDeadline(drainBudget).String())

		// Quoted WHERE IT NEEDS TO BE and only there. EscapeArg quotes an
		// argument only when it contains whitespace, so asserting everything
		// is quoted would fail on a correct registration.
		imagePath := registeredImagePath(t)
		assert.Contains(t, imagePath, g.configPath, "the config path did not survive registration")
		assert.NotContains(t, imagePath, `""`, "the ImagePath is doubly quoted")

		if strings.Contains(g.binary, " ") {
			assert.Contains(t, imagePath, `"`+g.binary+`"`, "a spaced exe path must be quoted")
		}
	})

	t.Run("6: it starts and serves", func(t *testing.T) {
		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", time.Minute)
		waitReady(t, g.baseURL+"/api/v1/health")
	})

	t.Run("7: the e2e suite passes against the service", func(t *testing.T) {
		runE2EAgainst(t, g)
	})

	t.Run("8: the log file records a service start", func(t *testing.T) {
		require.True(t, g.logContains(t, "SYS-001"), "no SYS-001 in %s", g.logFile)
		require.True(t, g.logContains(t, "runMode=service"),
			"the log does not say this was started by the SCM")
	})

	t.Run("9: the Event Log records the same start", func(t *testing.T) {
		entries := eventLog(t, 50)
		assert.Contains(t, entries, "runMode=service")
		// Its allocated numeric ID: what an Event Viewer filter matches on.
		assert.Regexp(t, `Id\s*:\s*1\b`, entries)
	})

	// --- Part B: the failure paths ---------------------------------------

	t.Run("10: a clean stop is NOT retried", func(t *testing.T) {
		// HERE, not after the failure steps. Recovery delays widen with the
		// failure count and the counter only resets after a day, so once
		// steps 11 to 13 have failed the service several times the applicable
		// delay is 60s — and a short wait could no longer tell "never
		// restarted" from "restarted later". Run while the counter is zero
		// and the first delay is 5s, and the wait is decisive.
		g.mustAdapter(t, "service", "stop")
		waitState(t, "STOPPED", time.Minute)

		time.Sleep(15 * time.Second)
		assert.Equal(t, "STOPPED", state(t),
			"the SCM restarted a service the operator deliberately stopped")

		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", time.Minute)
	})

	t.Run("11: a bad config fails fast and says why", func(t *testing.T) {
		g.mustAdapter(t, "service", "stop")
		waitState(t, "STOPPED", time.Minute)

		// An unreadable token store: a failure that happens AFTER the log
		// file is opened, so both sinks should carry it. The config is left
		// exactly as it was — only the store is corrupted.
		require.NoError(t, os.WriteFile(g.tokenStore, []byte("not a token store\n"), 0o600))

		started := time.Now()
		out, err := g.adapterCmd(t, "service", "start")
		elapsed := time.Since(started)

		require.Error(t, err, "a service with an unreadable token store must not start")

		// THE POINT OF THIS STEP. Before the fix, a start that could never
		// succeed ran the full four-minute transition timeout and then
		// reported a timeout rather than the failure.
		assert.Less(t, elapsed, 90*time.Second,
			"the start took %s: it is waiting out the transition timeout instead of "+
				"noticing the service stopped", elapsed)
		assert.Regexp(t, `exit code|stopped`, out+err.Error())

		assert.Contains(t, eventLog(t, 20), "SYS-005", "the Event Log must carry the reason")
		assert.True(t, g.logContains(t, "SYS-005"),
			"the log file must carry the startup failure too; it was written into a closed handle once")
	})

	t.Run("12: a start that fails is reported, not retried", func(t *testing.T) {
		// MEASURED, and it corrects what this milestone assumed. Phase -1 saw
		// the recovery schedule fire at 5s/10s/60s -- but there the service
		// reached Running and THEN exited non-zero. A service that fails
		// before it ever reports Running is a failed START: Windows returns
		// the error to whoever asked for it and does not put the service on
		// the recovery schedule.
		//
		// That is the better behaviour. An operator who typed `service start`
		// gets told why, instead of a service quietly looping in the
		// background. But it is the opposite of what the docs claimed, so it
		// is asserted here rather than left as folklore.
		before := countEventID(t, 5)

		time.Sleep(20 * time.Second)

		assert.Equal(t, before, countEventID(t, 5),
			"the SCM retried a service that never reached Running; if that is now true, "+
				"the note in docs/windows-service.md about failed starts needs revisiting")
		assert.Equal(t, "STOPPED", state(t))

		// Heal it for the steps that need a running service.
		require.NoError(t, os.Remove(g.tokenStore))
		g.token = mintToken(t, g.binary, g.tokenStore)
		g.mustAdapter(t, "service", "secure", "--config", g.configPath)
		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", 2*time.Minute)
		waitReady(t, g.baseURL+"/api/v1/health")
	})

	t.Run("13: an unclean death is retried too", func(t *testing.T) {
		pid := servicePID(t)
		require.NotZero(t, pid)

		_, _ = psErr(t, fmt.Sprintf("taskkill /F /PID %d", pid))

		// THIS is what the recovery schedule actually covers: a service that
		// was running and then died. Step 12 establishes the other half --
		// a failure during startup is reported to the caller instead.
		waitState(t, "RUNNING", 2*time.Minute)
		assert.NotEqual(t, pid, servicePID(t), "the SCM did not restart the killed process")
	})

	t.Run("14: a stop mid-drain waits instead of failing", func(t *testing.T) {
		g.startWithSlowBackend(t)

		// Recorded so step 15 can bound its search. Earlier steps already
		// wrote a clean SYS-004, and a whole-file scan would find that one
		// and pass without observing this drain at all.
		g.drainWindowStart = time.Now()

		// A request that will be in flight when the stop arrives.
		inFlight := beginSlowRequest(g)

		time.Sleep(2 * time.Second)

		// A service already at StopPending answers Control(Stop) with 1061.
		// Before that was tolerated, this errored out instead of waiting for
		// the drain it had asked for.
		_, err := g.adapterCmd(t, "service", "stop")
		require.NoError(t, err, "a stop issued during a drain must wait, not fail")

		waitState(t, "STOPPED", 2*time.Minute)
		<-inFlight
	})

	t.Run("15: the drain completed rather than being truncated", func(t *testing.T) {
		// Bounded to the stop in step 14. The backend stub sleeps for less
		// than the drain budget, so a correct drain finishes and reports
		// SYS-004; SYS-007 here would mean the wall was tighter than the
		// drain, which is what PreshutdownTimeout covering DrainDeadline
		// exists to prevent.
		window := g.logBetween(t, g.drainWindowStart, time.Now())

		assert.Contains(t, window, "SYS-004", "the drain in step 14 did not complete cleanly")
		assert.NotContains(t, window, "SYS-007",
			"SYS-007: the drain was cut off, so the SCM's wall is tighter than the drain budget")
	})

	// --- the lockdown, read back independently ---------------------------

	t.Run("16: the lockdown is real, read through Get-Acl", func(t *testing.T) {
		for _, path := range []string{g.configPath, g.tokenStore} {
			rules, protected, owner := acl(t, path)
			assertLockedDown(t, path, rules, protected, owner)
		}

		rules, _, _ := acl(t, filepath.Dir(g.logFile))

		var inheritable bool

		for _, r := range rules {
			if strings.Contains(r.InheritanceFlags, "ObjectInherit") {
				inheritable = true
			}
		}

		assert.True(t, inheritable,
			"the log directory's entries are not inheritable, so a log the adapter creates "+
				"would land on the parent's defaults")
	})

	t.Run("17: a log file the SERVICE created inherited it", func(t *testing.T) {
		// Not one an admin created. The whole reason the directory is secured
		// rather than the file is that the adapter makes the log at runtime.
		require.FileExists(t, g.logFile)

		rules, _, _ := acl(t, g.logFile)
		assertOnlyPolicyPrincipals(t, g.logFile, rules)
	})

	t.Run("18: a widened token store refuses the start", func(t *testing.T) {
		// The only case that walks GetAce, the inline SID read and
		// CheckGrants end to end.
		//
		// Stopped first: the refusal happens at STARTUP, so a service that is
		// already running would never evaluate the widened store and there
		// would be nothing to observe.
		g.mustAdapter(t, "service", "stop")
		waitState(t, "STOPPED", time.Minute)

		ps(t, fmt.Sprintf(`icacls '%s' /grant '%s:(W)'`, g.tokenStore, usersSID))

		_, err := g.adapterCmd(t, "service", "start")
		require.Error(t, err, "a token store writable by Users must refuse the start")
		assert.Contains(t, eventLog(t, 20), "S-1-5-32-545", "the refusal must name the offending SID")

		// And `service secure` repairs it.
		g.mustAdapter(t, "service", "secure", "--config", g.configPath)
		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", time.Minute)
	})

	// 18a-18c continue 18's technique -- provoke a refusal with a real
	// descriptor, observe it in the Event Log, repair with `service secure` --
	// against the three startup checks that had no host coverage. Lettered
	// rather than renumbered so the numbers below, which appear in the M4a
	// sign-off, keep meaning what they meant.

	t.Run("18a: a securable somebody else owns refuses the start", func(t *testing.T) {
		// The half of S1 that the access list cannot express. An owner holds
		// READ_CONTROL and WRITE_DAC implicitly, so a perfectly-locked list on
		// an object somebody else owns is a list they can replace whenever
		// they like -- which is what an unprivileged user gets by creating the
		// directory under C:\ProgramData before setup runs.
		//
		// The owner used is the INTERACTIVE ADMINISTRATOR, not some hostile
		// account, and that is the point: Windows' default owner policy is
		// "object creator", so this is the owner a directory an operator made
		// by hand actually has. It is outside {SYSTEM, Administrators} and
		// must be refused on a securable this tool locks down.
		g.mustAdapter(t, "service", "stop")
		waitState(t, "STOPPED", time.Minute)

		me := strings.TrimSpace(ps(t, `[Security.Principal.WindowsIdentity]::GetCurrent().User.Value`))
		require.NotEmpty(t, me)

		ps(t, fmt.Sprintf(`icacls '%s' /setowner '*%s'`, g.tokenStore, me))

		_, _, ownerBefore := acl(t, g.tokenStore)
		require.Equal(t, me, ownerBefore, "the test must actually have moved the owner")

		_, err := g.adapterCmd(t, "service", "start")
		require.Error(t, err, "a securable owned outside the policy must refuse the start")
		assert.Contains(t, eventLog(t, 20), "owned by", "the refusal must say the owner is the problem")

		// `service secure` takes it back -- it sets the owner as part of
		// applying the list, which is the whole reason the strict check is
		// answerable on what this tool touches.
		g.mustAdapter(t, "service", "secure", "--config", g.configPath)

		_, _, ownerAfter := acl(t, g.tokenStore)
		assert.Equal(t, administratorsSID, ownerAfter, "secure must take ownership back")

		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", time.Minute)
	})

	t.Run("18c: delete-child alone on the data directory refuses the start", func(t *testing.T) {
		// FILE_DELETE_CHILD is granted independently of DELETE and lets its
		// holder remove or rename any child whatever the child's own list
		// says -- so this alone takes out the token store or the executable.
		// It was missing from writeMask, which means a directory carrying only
		// this read as clean.
		//
		// The DIRECTORY, because the right is meaningless on a file.
		g.mustAdapter(t, "service", "stop")
		waitState(t, "STOPPED", time.Minute)

		logDir := filepath.Dir(g.logFile)
		ps(t, fmt.Sprintf(`icacls '%s' /grant '%s:(DC)'`, logDir, usersSID))

		_, err := g.adapterCmd(t, "service", "start")
		require.Error(t, err, "delete-child on the data directory must refuse the start")
		assert.Contains(t, eventLog(t, 20), "S-1-5-32-545", "the refusal must name the offending SID")

		g.mustAdapter(t, "service", "secure", "--config", g.configPath)
		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", time.Minute)
	})

	t.Run("19: service secure is idempotent", func(t *testing.T) {
		before, beforeProtected, beforeOwner := acl(t, g.configPath)

		g.mustAdapter(t, "service", "secure", "--config", g.configPath)

		after, afterProtected, afterOwner := acl(t, g.configPath)

		// A run that drifted would make the state of a host depend on how
		// many times somebody typed the command.
		assert.Equal(t, before, after)
		assert.Equal(t, beforeProtected, afterProtected)
		assert.Equal(t, beforeOwner, afterOwner)
	})

	// --- Part D: provisioning through `setup` -----------------------------
	//
	// Everything above drives `service install` against a config the gate
	// wrote by hand. These steps drive the ONE command an operator is meant to
	// run, and they exist because the two would otherwise drift: the gate
	// would keep proving a sequence nobody follows while the documented path
	// went unexercised.

	t.Run("20: setup provisions a host from nothing", func(t *testing.T) {
		g.mustAdapter(t, "service", "stop")
		g.mustAdapter(t, "service", "uninstall", "--yes")
		waitAbsent(t)

		// The gate's FIXED key, via a file, never --generate-namespace-key.
		// A generated key would make the identity assertion below pass
		// vacuously -- it would agree with whatever was just invented.
		require.NoError(t, os.WriteFile(g.keyFile, []byte(gateNamespaceKey), 0o600))

		out, err := g.adapterCmd(t, "setup",
			"--"+flagDataDir, g.setupDir,
			"--"+flagBinDir, filepath.Join(g.setupDir, "bin"),
			"--"+flagNoCopy,
			"--"+flagTokenLabel, "gate",
			"--"+flagNamespaceKeyFil, g.keyFile,
			"--"+config.FlagName(dhcpwindows.KeyServerName), gateServerName,
			"--"+config.FlagName(config.KeyPort), strconv.Itoa(g.setupPort),
			"--"+consentFlag)

		// Exit 4 is a legitimate success here: it means installed, running,
		// and the backend unhealthy. This host has a real DHCP server so 0 is
		// expected, but a gate that demanded it would fail for the backend
		// being briefly out rather than for anything setup did.
		require.True(t, err == nil || exitCode(err) == exitUnhealthy,
			"setup exited unexpectedly: %v\n%s", err, out)

		for _, step := range []string{"directory", "config", "token", "install", "start", "verify"} {
			assert.Contains(t, out, step, "setup did not report the %s step", step)
		}

		// The token reaches the operator exactly once, and the store keeps
		// only a hash -- so a run that minted one and did not show it has
		// destroyed a credential and occupied the label.
		assert.Contains(t, out, auth.TokenPrefix, "setup minted a token and never showed it")
	})

	t.Run("21: it registered and started a real service", func(t *testing.T) {
		waitState(t, "RUNNING", 2*time.Minute)

		status := g.mustAdapter(t, "service", "status")
		assert.Contains(t, status, "automatic")
		assert.Contains(t, status, g.binary, "setup registered a binary other than the one it ran from")
		assert.Contains(t, registeredImagePath(t), filepath.Join(g.setupDir, "config.toml"),
			"the registration does not point at the config setup wrote")
	})

	t.Run("22: the identity survived being provisioned from nothing", func(t *testing.T) {
		// THE assertion of this part. A config rendered by setup has to
		// produce the same wadaptIDs as one written by hand from the same key
		// and server name -- otherwise provisioning a host silently re-keys
		// it, every ID weave has seen moves, and the recreates that follow are
		// refused by Windows' one-scope-per-subnet rule.
		//
		// Read from the log rather than recomputed from the file, so what is
		// asserted is what the RUNNING service derived.
		want := dhcpwindows.NamespaceKeyFingerprint(gateNamespaceKey)

		logged := ps(t, fmt.Sprintf(
			`Select-String -Path '%s' -Pattern 'namespaceKeyFingerprint=(\S+)' | `+
				`Select-Object -Last 1 | ForEach-Object { $_.Matches[0].Groups[1].Value }`,
			filepath.Join(g.setupDir, "adapter.log")))

		assert.Equal(t, want, logged, "setup re-keyed the host: every wadaptID has moved")
	})

	t.Run("23: everything setup created is locked down", func(t *testing.T) {
		// Read back through Get-Acl, like step 16, rather than through our own
		// reader -- and asserting PROTECTED, because a file that merely
		// inherits the right entries is not the same as one that carries them.
		for _, path := range []string{
			g.setupDir,
			filepath.Join(g.setupDir, "config.toml"),
			filepath.Join(g.setupDir, "tokens.toml"),
		} {
			rules, protected, owner := acl(t, path)
			assertLockedDown(t, path, rules, protected, owner)
		}
	})

	t.Run("24: re-running setup changes nothing", func(t *testing.T) {
		// The promise the command is built on: safe to re-run. Every step must
		// report satisfied, and nothing may be rewritten -- an existing config
		// above all, since a rewrite is how a namespace key gets changed by
		// accident.
		before := ps(t, fmt.Sprintf(`(Get-FileHash '%s' -Algorithm SHA256).Hash`,
			filepath.Join(g.setupDir, "config.toml")))

		out, err := g.adapterCmd(t, "setup",
			"--"+flagDataDir, g.setupDir,
			"--"+flagBinDir, filepath.Join(g.setupDir, "bin"),
			"--"+flagNoCopy,
			"--"+flagTokenLabel, "gate",
			"--"+flagNamespaceKeyFil, g.keyFile,
			"--"+config.FlagName(dhcpwindows.KeyServerName), gateServerName,
			"--"+config.FlagName(config.KeyPort), strconv.Itoa(g.setupPort),
			"--"+consentFlag)

		require.True(t, err == nil || exitCode(err) == exitUnhealthy,
			"a re-run exited unexpectedly: %v\n%s", err, out)

		assert.NotContains(t, out, "pending", "a re-run still had work to do")
		assert.NotContains(t, out, auth.TokenPrefix, "a re-run minted a second token")

		after := ps(t, fmt.Sprintf(`(Get-FileHash '%s' -Algorithm SHA256).Hash`,
			filepath.Join(g.setupDir, "config.toml")))
		assert.Equal(t, before, after, "a re-run rewrote the configuration")
	})

	t.Run("25: a different server name is refused, not applied", func(t *testing.T) {
		// An existing configuration is never rewritten, and the refusal is
		// what stops a re-key arriving as a typo. The case-folded spelling is
		// accepted by step 24 above; this is a genuinely different host.
		out, err := g.adapterCmd(t, "setup", "--"+flagDryRun,
			"--"+flagDataDir, g.setupDir,
			"--"+flagTokenLabel, "gate",
			"--"+config.FlagName(dhcpwindows.KeyServerName), "somewhere-else.gate.test")

		require.Error(t, err, "a conflicting server name was accepted")
		assert.Contains(t, out+err.Error(), dhcpwindows.KeyServerName)

		// Neither half of the comparison may be printed: the same code path
		// carries identity.namespaceKey.
		assert.NotContains(t, out, gateNamespaceKey)
	})

	// --- Part C: teardown -------------------------------------------------

	t.Run("26: uninstall leaves nothing behind", func(t *testing.T) {
		g.mustAdapter(t, "service", "uninstall", "--yes")

		// Polled for actual absence rather than trusting the delete: Windows
		// removes a service only once the last handle closes.
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) && state(t) != "ABSENT" {
			time.Sleep(time.Second)
		}

		assert.Equal(t, "ABSENT", state(t))

		out := ps(t, fmt.Sprintf(
			`Test-Path 'HKLM:\SYSTEM\CurrentControlSet\Services\%s'`, serviceName))
		assert.Equal(t, "False", out, "the service's registry key survived the uninstall")

		// The registry key, not Get-WinEvent. Entries already written keep
		// their provider name after the source is deregistered, and whether
		// -ProviderName resolves against provider metadata or the log's
		// contents is exactly the kind of Windows detail this milestone has
		// been wrong about twice. The key either exists or it does not.
		sourceKey := ps(t, fmt.Sprintf(
			`Test-Path 'HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\%s'`,
			serviceName))
		assert.Equal(t, "False", sourceKey, "the Event Log source registration survived the uninstall")
	})

	t.Run("27: reinstall against the PROVISIONED config", func(t *testing.T) {
		// Not the gate's. A host left logging into this run's scratch
		// directory satisfies the letter of the invariant and not its point.
		g.mustAdapter(t, "service", "install", "--config", provisioned, "--"+consentFlag)
		g.mustAdapter(t, "service", "start")
		waitState(t, "RUNNING", 2*time.Minute)
	})
}

// assertLockedDown checks the full policy on a single target.
func assertLockedDown(t *testing.T, path string, rules []aclEntry, protected bool, owner string) {
	t.Helper()

	assert.True(t, protected, "%s still inherits: the wider parent grant survives", path)
	assertOnlyPolicyPrincipals(t, path, rules)

	// The one write authorised by the admin token rather than by the DACL, so
	// it is what proves install really had the rights it claimed.
	//
	// An EXACT SID, not a substring of a name. The old form matched
	// "administrator" anywhere in a lower-cased name, which the interactive
	// `WIN-01\Administrator` account satisfies as readily as the
	// Administrators group — so it could not see the one difference S1's owner
	// check is about, and it leaned on a locale-dependent string on a host
	// whose group is called "Administratoren".
	assert.Equal(t, administratorsSID, owner,
		"%s must be owned by the Administrators group, not by %q", path, owner)
}

// assertOnlyPolicyPrincipals checks that nobody outside the policy appears.
func assertOnlyPolicyPrincipals(t *testing.T, path string, rules []aclEntry) {
	t.Helper()

	require.NotEmpty(t, rules, "%s has no access rules at all", path)

	for _, r := range rules {
		assert.Contains(t, winsvc.LockdownGrantees(), r.SID,
			"%s grants %s rights %s", path, r.SID, r.FileSystemRights)
		// FILE_ALL_ACCESS renders as FullControl; GENERIC_ALL would render as
		// the raw number, which is why the mask is specific.
		assert.Contains(t, r.FileSystemRights, "FullControl", "%s: unexpected rights", path)
	}
}
