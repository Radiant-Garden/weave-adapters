// Package dhcpwindows is the Windows Server DHCP backend client: the adapter
// half of weave-adapter-dhcp-windows, and the first thing to live under
// internal/adapters/.
//
// It talks to the DhcpServer PowerShell module through powershell.exe, behind a
// narrow injectable runner. Narrow because the point of picking PowerShell
// first was that it stays swappable — dhcpsapi.dll via golang.org/x/sys/windows
// is the performance path if measurement ever justifies it, recorded so the
// seam is designed for it, not so it gets built.
//
// The package exports a concrete *Client and no interface. Go's idiom is to
// declare an interface where it is consumed, so the health probe and the scopes
// service each declare the one method they need. That is also what stops such
// an interface growing to mirror the implementation as write support lands.
//
// No value is interpolated into any script: every script body is a Go constant,
// and the values a command needs travel through the child process environment.
// That rule holds for the create path as it did when every command was a read.
//
// The account is *not* read-only and cannot be through this transport — the
// cmdlets reach DHCP through WMI, which gates on Administrators. See
// docs/dhcp-backend.md.
package dhcpwindows

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// Backend failure modes, kept distinct so a handler can map "the backend is
// unreachable" and "the backend spoke nonsense" to different problem types
// rather than collapsing both into one generic 502.
var (
	// ErrBackendUnavailable — the shell could not be run, or exited non-zero.
	ErrBackendUnavailable = errors.New("dhcp backend unavailable")
	// ErrBackendTimeout — the call exceeded its context deadline.
	ErrBackendTimeout = errors.New("dhcp backend timed out")
	// ErrBackendMalformed — the shell exited zero but its output could not be
	// decoded, including the empty-stdout case.
	ErrBackendMalformed = errors.New("dhcp backend returned malformed output")
	// ErrDuplicateWadaptID — two scopes derived the same identity. Detected
	// rather than tolerated; see ListScopes.
	ErrDuplicateWadaptID = errors.New("two scopes derived the same wadaptID")
)

// Client reads and creates scopes on a Windows DHCP server. It is safe for
// concurrent use: each call spawns its own process, and the one piece of
// mutable state it holds — the drift ledger — takes its own lock.
type Client struct {
	runner runner
	// commandTimeout bounds one backend invocation. It is applied here rather
	// than left to callers: exec.CommandContext only honours a deadline that
	// exists, so a caller passing a plain context would spawn a powershell.exe
	// nothing can reclaim, and cmd.WaitDelay would never come into play.
	commandTimeout time.Duration
	serverName     string
	namespaceKey   []byte
	// drift remembers each identity's last-seen attributes so a subnet that is
	// deleted and recreated can be noticed. It is the one piece of mutable
	// state on this client, carries its own lock, and is usable zero — which is
	// why *Client must not be copied, as go vet's copylocks enforces.
	drift driftLedger
}

// NewClient returns a client reading from the configured server.
//
// serverName and namespaceKey are the identity inputs, not connection details:
// serverName is the canonical provisioned name hashed into every wadaptID, and
// is deliberately separate from cfg.Server, which is only where to connect.
// Conflating the two is what made an os.Hostname() fallback look reasonable,
// and an identity that silently follows whatever the host is called re-keys the
// fleet on a rename.
func NewClient(cfg Config) *Client {
	return &Client{
		runner: execRunner{
			path:   cfg.PowerShellPath,
			server: cfg.Server,
		},
		commandTimeout: cfg.CommandTimeout,
		serverName:     cfg.ServerName,
		namespaceKey:   []byte(cfg.NamespaceKey),
	}
}

// bounded applies the per-invocation timeout to a call.
//
// Every backend call goes through here, so dhcp.commandTimeout is actually
// enforced rather than merely validated. It was neither applied nor reachable
// before: ListScopes handed the caller's context straight to the runner, so an
// operator raising the key — which BACKEND-101's own troubleshooting text
// advises — changed nothing at all, and a hung shell was bounded only by
// whatever deadline the caller happened to carry.
//
// A zero timeout means unbounded rather than instantly-expired. Config
// validation rejects a non-positive value, so the binary never sees one; this
// is for a Client built directly in a test, where a deadline of zero would
// otherwise cancel every call before it began.
func (c *Client) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.commandTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, c.commandTimeout)
}

// ListScopes returns every IPv4 scope on the server, each carrying its derived
// wadaptID, sorted by that ID.
//
// Sorting here rather than in the handler is what makes the pagination
// guarantee hold: the core cursor is a resume key, not an offset, so the sort
// and the resume must compare the same thing. Both compare the encoded wadaptID
// string — never the raw HMAC bytes, never a decoded form. Held to that,
// consistency is structural.
//
// It is one PowerShell spawn, and the whole read path: read, derive, serve. No
// writes, no re-read, no verification, no singleflight.
func (c *Client) ListScopes(ctx context.Context) ([]Scope, error) {
	ctx, cancel := c.bounded(ctx)
	defer cancel()

	stdout, stderr, err := c.runner.run(ctx, listScopesScript, nil)
	if err != nil {
		return nil, c.backendError(ctx, opListScopes, runError(err, stderr))
	}

	scopes, err := decodeScopes(stdout, stderr)
	if err != nil {
		return nil, c.backendError(ctx, opListScopes, err)
	}

	if err := c.identify(scopes); err != nil {
		return nil, c.backendError(ctx, opListScopes, err)
	}

	c.reportDrift(ctx, scopes)

	// One comparator, on the encoded string, matching the resume comparison.
	slices.SortFunc(scopes, func(a, b Scope) int {
		return cmp.Compare(a.WadaptID, b.WadaptID)
	})

	return scopes, nil
}

// reportDrift records the listing against the ledger and emits DHCP-002 for
// every identity whose scope changed materially.
//
// Called from every path that produces identified scopes, because a listing is
// a listing however it was obtained — and a create is the operation most likely
// to reuse a subnet, so observing it is the point rather than an extra.
//
// One event per drifted identity, not one summarizing all of them: each names a
// distinct wadaptID an operator has to reconcile separately, and a single line
// listing five would be filtered as one.
func (c *Client) reportDrift(ctx context.Context, scopes []Scope) {
	for _, report := range c.drift.observe(scopes) {
		events.Emit(ctx, adapterevents.DHCP002,
			"wadaptId", report.wadaptID,
			"scopeId", report.scopeID,
			"changed", strings.Join(changedFields(report.was, report.now), ", "),
		)
	}
}

// changedFields names the attributes that differ, so the event says what moved
// rather than only that something did.
//
// Values are deliberately not logged. A scope name is operator-supplied free
// text and the ranges are network topology; naming the fields is enough to
// decide whether this was an edit or a recreate, and the DHCP server's own
// change history holds the values.
func changedFields(was, now fingerprint) []string {
	var changed []string

	if was.name != now.name {
		changed = append(changed, "name")
	}

	if was.startRange != now.startRange {
		changed = append(changed, "startRange")
	}

	if was.endRange != now.endRange {
		changed = append(changed, "endRange")
	}

	if was.subnetMask != now.subnetMask {
		changed = append(changed, "subnetMask")
	}

	if was.leaseDurationSeconds != now.leaseDurationSeconds {
		changed = append(changed, "leaseDurationSeconds")
	}

	return changed
}

// Operation labels carried by BACKEND-101, so an operator can tell a failing
// health poll from a failing request in the log.
const (
	opListScopes  = "listScopes"
	opProbe       = "probe"
	opCreateScope = "createScope"
)

// backendError emits BACKEND-101 and returns the error unchanged.
//
// Every backend failure funnels through here, which is what makes the client
// the single owner of that ID: it is the layer that knows whether the shell
// could not start, exited non-zero, timed out, or spoke nonsense. Callers above
// — the health probe, and resource handlers once they exist — trust this event
// and never re-emit, so one failure produces one log line rather than one per
// layer it passes through.
//
// It returns the error rather than swallowing it: handlers still return errors,
// and apierror.WriteError remains the one place that logs *and* responds. This
// logs the backend fact; that logs the client-facing outcome.
//
// A persistently unreachable backend therefore logs one ERROR per failed call,
// including one per health poll that misses the probe cache. That repetition is
// accepted rather than overlooked. The rate is bounded — the probe runs at most
// once per health.probeCacheTTL, and only when someone actually polls health —
// and the line carries the shell's own stderr, which is the single most useful
// diagnostic and otherwise reaches the log nowhere: HLT-001 fires once on the
// status transition and names no cause, and the health response carries the
// detail only to whoever polled it. Suppressing the repeat would need
// last-failure state in a package that is deliberately stateless, to save an
// operator from log lines that are telling them the truth.
func (c *Client) backendError(ctx context.Context, operation string, err error) error {
	events.Emit(ctx, adapterevents.BACKEND101, "operation", operation, "error", err.Error())

	return err
}

// identify derives each scope's wadaptID and rejects a collision.
//
// A 64-bit truncated HMAC can collide. At DHCP scale the probability is
// negligible (~n²/2⁶⁵), but the consequence is not: two scopes sharing an ID is
// also a repeated pagination sort key, which violates the unique-sort-key
// requirement and would silently drop the remainder of a walk at a page
// boundary. One map pass converts an unbounded silent failure into a loud one —
// the same trade $ErrorActionPreference = 'Stop' and the empty-stdout rule make.
func (c *Client) identify(scopes []Scope) error {
	seen := make(map[string]string, len(scopes))

	for i := range scopes {
		id := deriveWadaptID(c.namespaceKey, c.serverName, scopes[i].ScopeID)

		if other, dup := seen[id]; dup {
			return fmt.Errorf("%w: scopes %s and %s both derive %s",
				ErrDuplicateWadaptID, other, scopes[i].ScopeID, id)
		}

		seen[id] = scopes[i].ScopeID
		scopes[i].WadaptID = id
		scopes[i].AddressFamily = AddressFamilyIPv4
	}

	return nil
}

// decodeScopes reads the projection's JSON.
//
// Empty stdout is an error, not an empty list. A DHCP server with no scopes
// emits a valid "[ ]" — verified on the host — so the only remaining way to get
// zero bytes is a failure: a killed process, a crashed shell, output swallowed
// by a profile. Parsing that as "zero scopes" would reintroduce exactly the
// silent-wrong-answer class the Stop preference exists to close, and weave
// seeing a healthy adapter report no scopes could reasonably conclude they were
// all deleted.
//
// Both guards are needed and neither subsumes the other: Stop catches the
// failures that still exit zero, this catches the ones that produce no output.
func decodeScopes(stdout, stderr []byte) ([]Scope, error) {
	if strings.TrimSpace(string(stdout)) == "" {
		return nil, fmt.Errorf("%w: no output%s", ErrBackendMalformed, stderrContext(stderr))
	}

	var scopes []Scope
	if err := json.Unmarshal(stdout, &scopes); err != nil {
		return nil, fmt.Errorf("%w: %w%s", ErrBackendMalformed, err, stderrContext(stderr))
	}

	// A bare "null" is not an empty list.
	//
	// json.Unmarshal("null", &scopes) leaves a nil slice and returns no error,
	// so it walks past the empty-stdout guard above (the text is not empty) and
	// past the per-element loop below (there are no elements) to be served as
	// "this server has zero scopes" — the precise wrong answer the empty-stdout
	// rule exists to prevent, arriving by a different door. A server with no
	// scopes emits "[ ]", which decodes to a non-nil empty slice.
	if scopes == nil {
		return nil, fmt.Errorf("%w: output was null%s", ErrBackendMalformed, stderrContext(stderr))
	}

	for i, s := range scopes {
		if err := validateScope(i, s); err != nil {
			return nil, fmt.Errorf("%w%s", err, stderrContext(stderr))
		}
	}

	return scopes, nil
}

// validateScope rejects an element that decoded cleanly but cannot be a scope.
//
// Two distinct failures, both of which produce a *well-formed* wadaptID if left
// alone, which is what makes them dangerous rather than merely wrong:
//
//   - No scopeId. "[null]" unmarshals into one zero-valued Scope with no error,
//     and a zero-valued Scope derives a perfectly valid ID, so the adapter would
//     serve a phantom scope that exists nowhere and weave would reconcile
//     against it.
//   - A scopeId that is not an IPv4 address. This is the Go-side tripwire for
//     the plan's -Depth trap: at the default depth of 2, PowerShell serializes
//     nested values as the literal string "System.Object[]", which decodes into
//     a string field without complaint and derives an ID just as happily. The
//     script passes -Depth explicitly so it should never arrive, but a future
//     projection edit that reintroduces the trap would otherwise fail silently.
//
// Only scopeId is validated. It is the derivation input and the natural key, so
// a wrong value there is a wrong *identity* rather than a wrong attribute — and
// because a depth regression corrupts every IPAddress field at once, checking
// the one that matters most also catches the class. The remaining fields are
// data, and inventing required-ness for them would be validation nobody asked
// for.
func validateScope(index int, s Scope) error {
	if s.ScopeID == "" {
		return fmt.Errorf("%w: scope at index %d has no scopeId", ErrBackendMalformed, index)
	}

	addr, err := netip.ParseAddr(s.ScopeID)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("%w: scope at index %d has scopeId %q, which is not an IPv4 address",
			ErrBackendMalformed, index, s.ScopeID)
	}

	return nil
}

// runError classifies a runner failure and attaches stderr as context.
//
// stderr's mere presence is deliberately *not* an independent failure. Under
// -NoProfile -NonInteractive with the Stop preference it is probably clean, but
// PS 5.1 renders several streams in ways that surprise, and if anything benign
// ever landed there every request would fail. Until that is verified silent
// across a successful run on a real host, non-zero exit and decode failure are
// the errors, and stderr is evidence attached to them.
func runError(err error, stderr []byte) error {
	if errors.Is(err, ErrBackendTimeout) {
		return fmt.Errorf("%w%s", err, stderrContext(stderr))
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%w: powershell exited %d%s",
			ErrBackendUnavailable, exitErr.ExitCode(), stderrContext(stderr))
	}

	return fmt.Errorf("%w: %w%s", ErrBackendUnavailable, err, stderrContext(stderr))
}

// stderrContext renders stderr for an error message, bounded so a shell that
// produced a wall of text cannot produce an unbounded log line.
func stderrContext(stderr []byte) string {
	const maxStderr = 512

	trimmed := strings.TrimSpace(string(stderr))
	if trimmed == "" {
		return ""
	}

	// Truncated by runes, not bytes. Everything in this package is aimed at
	// non-ASCII output surviving intact, and splitting a rune here would put a
	// replacement character into the error describing that very problem.
	if utf8.RuneCountInString(trimmed) > maxStderr {
		trimmed = string([]rune(trimmed)[:maxStderr]) + "..."
	}

	return ": " + trimmed
}
