package auth

import (
	"fmt"
	"time"
)

// ExpiryInDays returns an Expiry the given number of days after from, refusing
// one the store could not write back.
//
// Asking the expiry to render is the bound, rather than a ceiling invented
// here: a value large enough to push the year past four digits — or far enough
// to wrap it negative — marshals to something UnmarshalText will not parse, so
// Save would produce a token file that every later Load rejects. Checking at
// construction means the limit can never drift from the one the store actually
// enforces, and the caller hears about the number it was given instead of a
// marshalling failure three steps later.
//
// days must be positive. "Never expires" is the absence of an Expiry, not a
// zero one, and which input spells that is the caller's convention rather than
// this package's.
func ExpiryInDays(from time.Time, days int) (*Expiry, error) {
	if days < 1 {
		return nil, fmt.Errorf("an expiry needs a positive number of days, got %d", days)
	}

	// UTC first, so the stored timestamp does not carry the offset of whichever
	// shell happened to run the command.
	expiry := NewExpiry(from.UTC().AddDate(0, 0, days))

	// Returned as-is rather than wrapped. MarshalText's message already names
	// the offending year, which is the whole actionable part, and the caller
	// is the one that knows what to call the input — "--expires-in-days" to an
	// operator, something else to setup. A wrap here would put this package's
	// vocabulary between the two.
	if _, err := expiry.MarshalText(); err != nil {
		return nil, err
	}

	return expiry, nil
}

// Mint generates a token, records its hash under label, and returns the token.
// A nil expiresAt means it never expires.
//
// The caller must Save the store afterwards, and must show the returned token
// to the operator exactly once — the store keeps only a hash, so nothing can
// recover it.
//
// Save deliberately stays with the caller rather than happening here. The
// ordering is the point: Add runs first, so a duplicate label fails without
// the file being touched at all, and a Mint that saved itself would hide that
// from every call site. It also leaves the caller free to mint into a store it
// is going to write somewhere else.
func (s *Store) Mint(label string, now time.Time, expiresAt *Expiry) (string, error) {
	token, err := Generate()
	if err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}

	entry := Entry{
		Label:     label,
		Hash:      Hash(token),
		CreatedAt: now.UTC(),
		ExpiresAt: expiresAt,
	}

	if err := s.Add(entry); err != nil {
		return "", err
	}

	return token, nil
}
