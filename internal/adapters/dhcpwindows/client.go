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
// M3a is backend-read-only: every command here reads, no value is interpolated
// into any script, and a read-only service account serves every endpoint.
package dhcpwindows

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"unicode/utf8"
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

// Client reads scopes from a Windows DHCP server. It is safe for concurrent
// use: it holds only configuration, and each call spawns its own process.
type Client struct {
	runner       runner
	serverName   string
	namespaceKey []byte
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
		serverName:   cfg.ServerName,
		namespaceKey: []byte(cfg.NamespaceKey),
	}
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
	stdout, stderr, err := c.runner.run(ctx, listScopesScript)
	if err != nil {
		return nil, runError(err, stderr)
	}

	scopes, err := decodeScopes(stdout, stderr)
	if err != nil {
		return nil, err
	}

	if err := c.identify(scopes); err != nil {
		return nil, err
	}

	// One comparator, on the encoded string, matching the resume comparison.
	slices.SortFunc(scopes, func(a, b Scope) int {
		return cmp.Compare(a.WadaptID, b.WadaptID)
	})

	return scopes, nil
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

	return scopes, nil
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
