// Package pagination implements the one pagination scheme every adapter
// offers: opaque `pageToken` cursors with a clamped `pageSize`, and the
// `{items, nextPageToken}` collection envelope.
//
// There is deliberately no offset scheme, ever — not even where a backend could
// support it. Two schemes would mean every client, every spec component, and
// every adapter has to pick one, and offsets silently skip or repeat rows when
// the underlying collection changes between pages. Cursors resume *after a
// key*, so a page boundary means the same thing whether or not the collection
// moved underneath it.
//
// "Opaque" is a contract, not encryption: a token is base64url over a small
// JSON cursor, and anyone can decode it. It means clients must never construct
// or interpret one — only echo back a nextPageToken this endpoint minted. What
// the token protects against is *misinterpretation*, not forgery, and that is
// the right target: the cursor only names a position inside a collection the
// caller is already authorized to read, so a forged token grants nothing that
// a different page of the same list would not.
//
// That argument holds only while handlers keep their side of it:
//
//   - Treat Params.After as an **opaque comparison key**. It is
//     attacker-controlled — a forged token can carry any bytes up to
//     MaxKeyBytes — and it arrives through a 400-guarded Parse, so it is easy
//     to mistake for validated input. Compare it against stored keys; never
//     interpolate it into a query, a command line, or a PowerShell argument.
//   - Order the listing by the same key the token carries, or a resumed page
//     means nothing.
package pagination

import (
	"encoding/base64"
	"encoding/json"
)

// tokenVersion is the cursor wire format. It is checked on decode so a future
// change to the cursor struct rejects old tokens with a 400 rather than
// silently reading a field that has changed meaning.
const tokenVersion = 1

// MaxKeyBytes bounds the resume key a token may carry. Keys are resource
// identifiers, so the real ones are tens of bytes; the cap exists because the
// decoded key is attacker-controlled and reaches a handler looking like
// validated input. A token over the cap is rejected rather than truncated —
// truncating would hand back a *different* key that still looked valid.
const MaxKeyBytes = 1024

// cursor is the decoded content of a page token. Keys are short because the
// token travels in a query string on every page of every poll.
type cursor struct {
	// Version is the wire format; see tokenVersion.
	Version int `json:"v"`
	// Scope names the collection that minted the token. It is what makes a
	// token from another endpoint a validation error instead of a nonsense
	// resume key applied to the wrong list.
	Scope string `json:"s"`
	// After is the key of the last item on the previous page. The next page
	// starts at the first item ordered after it.
	//
	// It is []byte rather than string so encoding/json base64s it, which makes
	// the round-trip lossless for *any* key. A string field would be silently
	// corrupted: json.Marshal does not reject invalid UTF-8, it substitutes
	// U+FFFD, so a key carrying raw bytes — a vendor field or an option blob
	// read from a Windows API — would decode to a different key with no error
	// at all, and the handler would resume from the wrong position.
	After []byte `json:"k"`
}

// encodeToken renders a cursor as an opaque token. RawURLEncoding is used so
// the token is query-safe with no escaping and no '=' padding that a client
// might trim in transit.
func encodeToken(scope, after string) string {
	// The cursor is a fixed three-field struct of JSON-safe types, so encoding
	// cannot fail.
	encoded, _ := json.Marshal(cursor{Version: tokenVersion, Scope: scope, After: []byte(after)})

	return base64.RawURLEncoding.EncodeToString(encoded)
}

// decodeToken returns the resume key carried by token, or ok=false if the token
// is not one this scope minted.
//
// Every rejection reason collapses into one bool on purpose. A client has no
// action to take that differs between the four ways this can fail — not base64,
// not JSON, wrong version, or a token from /leases sent to /scopes. In every
// case the only correct behaviour is to restart the listing with no token, and
// reporting which check failed would describe our encoding to anyone probing it.
//
// Structural validation is all the tamper-evidence this needs, and it is
// stronger than it looks: random or edited bytes essentially never decode to
// valid JSON carrying both the current version and this endpoint's scope. A
// checksum or HMAC would add a key to manage and a token lifetime to reason
// about, buying no protection that matters — see the package comment on why
// forgery is not the threat here.
func decodeToken(token, scope string) (after string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", false
	}

	var decoded cursor
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", false
	}

	if decoded.Version != tokenVersion || decoded.Scope != scope {
		return "", false
	}

	// An absent, null, or empty key is a rejection, not a first page.
	//
	// encoding/json leaves a missing or null "k" as a nil slice with no error,
	// so without this check the cursor {"v":1,"s":"leases"} — which anyone can
	// craft, and which a truncated or half-built token can produce — would
	// decode happily to an empty After and restart the listing from row one.
	// That is the silent full-scan this package exists to make impossible: a
	// token we never minted has to be a 400, not a quiet reset.
	if len(decoded.After) == 0 {
		return "", false
	}

	// A key this long is not one we minted. Bounding it here keeps the promise
	// the package comment makes to handler authors — that After is a modest,
	// opaque comparison key — from being documentation only.
	if len(decoded.After) > MaxKeyBytes {
		return "", false
	}

	return string(decoded.After), true
}
