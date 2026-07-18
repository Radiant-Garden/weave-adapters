package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
func (e Expiry) MarshalText() ([]byte, error) {
	return []byte(time.Time(e).Format(time.RFC3339)), nil
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
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", path, err)
	}

	var store Store
	if err := toml.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parsing token file %q: %w", path, err)
	}

	return &store, nil
}

// Save writes the store atomically: a temp file in the destination directory is
// written, permissioned, and renamed over the target, so an interrupted write
// can never leave a half-written token file that locks every caller out.
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

	return nil
}

// writeAndClose writes data to f and closes it, reporting whichever step fails.
// The close error matters: a deferred close would discard a failed flush, and
// the rename would then publish a truncated file.
func writeAndClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("writing %q: %w", f.Name(), err)
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
