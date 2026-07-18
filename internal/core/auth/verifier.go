package auth

import (
	"crypto/subtle"
	"errors"
	"time"
)

// Verification failures. They are typed so the middleware can give each its own
// operator diagnostic while returning one indistinguishable response.
var (
	// ErrUnknownToken means the presented token matches no configured entry.
	ErrUnknownToken = errors.New("token is not recognized")
	// ErrTokenExpired means the token is configured but past its expiry.
	ErrTokenExpired = errors.New("token has expired")
)

// Verifier checks presented tokens against the configured set. It is read-only
// after construction and safe for concurrent use — tokens are read once at
// startup and never reloaded, so there is nothing to lock.
type Verifier struct {
	entries []Entry
	now     func() time.Time
}

// NewVerifier returns a Verifier over the given entries.
func NewVerifier(entries []Entry) *Verifier {
	return &Verifier{
		entries: entries,
		now:     time.Now,
	}
}

// Len reports how many tokens are configured. Startup uses it to refuse an
// empty allow-list, which would reject every request.
func (v *Verifier) Len() int { return len(v.entries) }

// Verify resolves a presented token to its entry.
//
// Comparison runs over the hashes, not the tokens: a fixed-length compare
// cannot leak the credential's length, and the adapter never holds a usable
// token to compare against in the first place. Every candidate is examined even
// after a match so the work does not depend on where in the list the token sits.
func (v *Verifier) Verify(token string) (Entry, error) {
	presented := []byte(Hash(token))

	var (
		matched Entry
		found   bool
	)

	for _, entry := range v.entries {
		if subtle.ConstantTimeCompare(presented, []byte(entry.Hash)) == 1 {
			matched = entry
			found = true
		}
	}

	if !found {
		return Entry{}, ErrUnknownToken
	}

	if matched.Expired(v.now()) {
		// Distinguished from unknown for the operator log only. The caller gets
		// the same answer either way — saying "expired" would confirm that a
		// guessed token exists.
		return matched, ErrTokenExpired
	}

	return matched, nil
}
