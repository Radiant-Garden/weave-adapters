package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// storeFileMode keeps the token file owner-only. It holds hashes rather than
// tokens, so a leak is not fatal — this is hygiene, not the security boundary.
// Windows largely ignores Unix permission bits; the file's protection there is
// the directory ACL.
const storeFileMode = 0o600

// labelPattern constrains labels to what is safe to render in a log line, an
// event field, and a TOML key.
var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// hashPattern is the stored form Hash produces. Checked on load because an entry
// whose hash is not this shape can never match a presented token: it is a token
// the operator believes is live and that every request will reject.
var hashPattern = regexp.MustCompile(`^` + hashAlgorithm + `:[0-9a-f]{64}$`)

// Errors returned by Store operations. They are typed so a CLI can tell an
// operator mistake (duplicate, unknown) from an I/O failure.
var (
	// ErrDuplicateLabel means a token with that label already exists. Adding
	// never overwrites: silently replacing a token would revoke a live
	// credential with no trace.
	ErrDuplicateLabel = errors.New("a token with that label already exists")
	// ErrUnknownLabel means no token with that label is in the store.
	ErrUnknownLabel = errors.New("no token with that label")
	// ErrInvalidLabel means the label is empty or uses disallowed characters.
	ErrInvalidLabel = errors.New("label must be 1-64 chars of letters, digits, '-' or '_', starting alphanumeric")
	// ErrInvalidHash means a stored hash is not in the form Hash produces, so it
	// could never match a presented token.
	ErrInvalidHash = errors.New("hash must be " + hashAlgorithm + ": followed by 64 lowercase hex digits")
)

// Expiry is an optional timestamp that survives a TOML round-trip.
//
// It exists because go-toml/v2 handles an optional time badly in both obvious
// spellings, and each failure is silent:
//   - *time.Time marshals to a quoted string, then refuses to unmarshal that
//     string back into the same field ("cannot decode TOML string").
//   - time.Time with omitempty drops the field even when it holds a real time,
//     so a saved expiry vanishes on the next write.
//
// Marshaling through text sidesteps both. Do not "simplify" this to a bare
// *time.Time — the round-trip test is what catches the regression.
type Expiry time.Time

// NewExpiry returns an Expiry for t, suitable for Entry.ExpiresAt.
func NewExpiry(t time.Time) *Expiry {
	e := Expiry(t)

	return &e
}

// Time returns the underlying timestamp.
func (e Expiry) Time() time.Time { return time.Time(e) }

// MarshalText renders the expiry as RFC 3339.
//
// It refuses a year outside RFC 3339's four digits rather than writing one.
// Format widens the year field past four digits, but the strict Parse in
// UnmarshalText will not read it back — so writing it would produce a token file
// that every later Load rejects: list, gen, revoke, and server startup alike,
// until someone hand-edits the file. Failing the Save that would have created it
// keeps the damage to the one command that asked for it.
func (e Expiry) MarshalText() ([]byte, error) {
	t := time.Time(e)
	if year := t.Year(); year < 0 || year > 9999 {
		return nil, fmt.Errorf("expiry year %d is outside RFC 3339's four-digit range", year)
	}

	return []byte(t.Format(time.RFC3339)), nil
}

// UnmarshalText parses an RFC 3339 expiry, so a hand-edited garbage value fails
// loudly at load instead of silently reading as "never expires".
func (e *Expiry) UnmarshalText(text []byte) error {
	parsed, err := time.Parse(time.RFC3339, string(text))
	if err != nil {
		return fmt.Errorf("parsing expiresAt %q: %w", text, err)
	}

	*e = Expiry(parsed)

	return nil
}

// Entry is one accepted token as stored on disk. The token itself is never
// stored — only its hash — so this file is not a credential.
type Entry struct {
	// Label identifies the token to operators and becomes the caller subject on
	// every event the token's requests emit.
	Label string `toml:"label"`
	// Hash is the token's stored form (see Hash).
	Hash string `toml:"hash"`
	// CreatedAt is when the token was minted.
	CreatedAt time.Time `toml:"createdAt"`
	// ExpiresAt is when the token stops being accepted. Nil means it never
	// expires.
	ExpiresAt *Expiry `toml:"expiresAt,omitempty"`
}

// Expired reports whether the entry is past its expiry at the given time.
// Expiry is inclusive: a token is expired at the instant it expires.
func (e Entry) Expired(now time.Time) bool {
	return e.ExpiresAt != nil && !now.Before(e.ExpiresAt.Time())
}

// Store is the set of accepted tokens, as persisted in the token file.
type Store struct {
	Tokens []Entry `toml:"tokens"`
}

// Load reads the token file. A missing file is reported as fs.ErrNotExist so
// callers can distinguish "no tokens yet" from an unreadable file.
//
// The file is hand-editable, so its contents get the same invariants Add
// enforces rather than being trusted for having parsed. Failing the whole load
// is deliberate: a token file nobody can vouch for is not one to start serving
// against, and the error names the entry to fix.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", path, err)
	}

	var store Store
	if err := toml.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parsing token file %q: %w", path, err)
	}

	if err := store.validate(); err != nil {
		return nil, fmt.Errorf("validating token file %q: %w", path, err)
	}

	return &store, nil
}

// validate checks every entry against the invariants Add would have applied.
//
// Duplicate labels are the reason this is not merely tidiness: Revoke removes
// the first match and returns, so the CLI would report success while the second
// entry — and its live hash — stayed in the store. An unvalidated label also
// flows straight into the caller subject on every event the token's requests
// emit, which is exactly what labelPattern exists to keep safe.
func (s *Store) validate() error {
	seen := make(map[string]struct{}, len(s.Tokens))

	for i, entry := range s.Tokens {
		if !labelPattern.MatchString(entry.Label) {
			return fmt.Errorf("entry %d: %w: %q", i, ErrInvalidLabel, entry.Label)
		}

		if _, duplicate := seen[entry.Label]; duplicate {
			return fmt.Errorf("entry %d: %w: %q", i, ErrDuplicateLabel, entry.Label)
		}

		seen[entry.Label] = struct{}{}

		if !hashPattern.MatchString(entry.Hash) {
			return fmt.Errorf("entry %d (label %q): %w", i, entry.Label, ErrInvalidHash)
		}
	}

	return nil
}

// Save writes the store atomically: a temp file in the destination directory is
// written, flushed to disk, permissioned, and renamed over the target, so an
// interrupted write can never leave a half-written token file that locks every
// caller out.
//
// The fsyncs are what make that true of a power loss and not merely of a killed
// process. Without the file sync the rename can reach disk before the bytes,
// leaving a zero-length tokens.toml and no way in; without the directory sync
// the rename itself can be lost, which for a revoke means a token the operator
// was told is dead comes back live.
func (s *Store) Save(path string) error {
	data, err := toml.Marshal(s)
	if err != nil {
		return fmt.Errorf("encoding token file: %w", err)
	}

	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tokens-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}

	tmpName := tmp.Name()

	// Best-effort cleanup: after a successful rename there is nothing to remove.
	defer func() { _ = os.Remove(tmpName) }()

	if err := writeAndClose(tmp, data); err != nil {
		return err
	}

	// Before the rename, so the file is never briefly world-readable.
	if err := os.Chmod(tmpName, storeFileMode); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing token file %q: %w", path, err)
	}

	return syncDir(dir)
}

// syncDir flushes a directory entry so a completed rename survives a power loss.
//
// Windows has no equivalent operation: opening the directory succeeds, but
// Sync on that handle fails with ERROR_ACCESS_DENIED, so there is nothing to
// attempt there. The rename has already succeeded by this point, so what
// Windows loses is the durability upgrade, not the write.
//
// A failure to open the directory is not reported for the same reason. A failed
// Sync on a handle we did open is reported, except where the platform rejects
// the operation outright.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	f, err := os.Open(dir) //nolint:gosec // the destination directory, derived from the caller's own path
	if err != nil {
		// The rename already succeeded; only the durability upgrade is lost.
		return nil
	}

	defer func() { _ = f.Close() }()

	if err := f.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("syncing directory %q: %w", dir, err)
	}

	return nil
}

// writeAndClose writes data to f, flushes it to disk, and closes it, reporting
// whichever step fails. The close error matters: a deferred close would discard
// a failed flush, and the rename would then publish a truncated file.
func writeAndClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("writing %q: %w", f.Name(), err)
	}

	// Before the close, and before the rename that follows it: a rename that
	// reaches disk ahead of the bytes publishes a zero-length token file, which
	// locks every caller out until the credentials are re-minted.
	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("syncing %q: %w", f.Name(), err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", f.Name(), err)
	}

	return nil
}

// Add appends an entry. It refuses a duplicate label rather than overwriting,
// since an overwrite would silently revoke a token that is still in use.
func (s *Store) Add(entry Entry) error {
	if !labelPattern.MatchString(entry.Label) {
		return fmt.Errorf("%w: %q", ErrInvalidLabel, entry.Label)
	}

	if _, found := s.Find(entry.Label); found {
		return fmt.Errorf("%w: %q", ErrDuplicateLabel, entry.Label)
	}

	s.Tokens = append(s.Tokens, entry)

	return nil
}

// Revoke removes the entry with the given label.
func (s *Store) Revoke(label string) error {
	for i, entry := range s.Tokens {
		if entry.Label == label {
			s.Tokens = append(s.Tokens[:i], s.Tokens[i+1:]...)

			return nil
		}
	}

	return fmt.Errorf("%w: %q", ErrUnknownLabel, label)
}

// Find returns the entry with the given label.
func (s *Store) Find(label string) (Entry, bool) {
	for _, entry := range s.Tokens {
		if entry.Label == label {
			return entry, true
		}
	}

	return Entry{}, false
}
