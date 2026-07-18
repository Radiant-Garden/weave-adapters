// Package pagination implements the one pagination scheme every adapter
// offers: opaque `pageToken` cursors with a clamped `pageSize`, and the
// `{items, nextPageToken}` collection envelope.
//
// There is no offset scheme, ever — not even where a backend could support it.
// Offsets silently skip or repeat rows when the collection changes between
// pages; cursors resume after a key, so a page boundary survives that.
//
// "Opaque" is a contract, not encryption: a token is base64url over a small
// JSON cursor and anyone can decode it. It means clients must never construct
// or interpret one, only echo back a nextPageToken this endpoint minted. The
// token guards against misinterpretation, not forgery — a forged one names a
// position in a collection the caller may already read.
//
// Handlers must hold up two ends of that:
//
//   - Treat Params.After as an opaque comparison key. It is
//     attacker-controlled up to MaxKeyBytes and arrives through a 400-guarded
//     Parse, so it is easy to mistake for validated input. Compare it against
//     stored keys; never interpolate it into a query or command line.
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

// MaxKeyBytes bounds the resume key a token may carry; real keys are resource
// identifiers tens of bytes long. A token over the cap is rejected rather than
// truncated, since truncating would yield a different key that still looked
// valid.
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
	// After is the key of the last item on the previous page.
	//
	// []byte rather than string so encoding/json base64s it and the round-trip
	// is lossless for any key. A string field is silently corrupted:
	// json.Marshal substitutes U+FFFD for invalid UTF-8 instead of erroring, so
	// a key of raw bytes would decode to a different key and resume from the
	// wrong position.
	After []byte `json:"k"`
}

// maxTokenLen returns the longest token this scope can legitimately have minted.
//
// A MaxKeyBytes key is base64'd by encoding/json inside the cursor, so the JSON
// carries ceil(MaxKeyBytes/3)*4 bytes for the key, plus the scope and the fixed
// structure; the whole document is then base64'd again. The slack covers the
// field names, braces, quotes and the version number.
func maxTokenLen(scope string) int {
	const cursorOverhead = 64

	keyJSON := (MaxKeyBytes+2)/3*4 + 2

	return base64.RawURLEncoding.EncodedLen(keyJSON + len(scope) + cursorOverhead)
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
// Every rejection collapses into one bool: the client's only recovery is the
// same for all four failures (not base64, not JSON, wrong version, foreign
// scope), and naming which check failed would describe the encoding to a prober.
//
// Structural validation is the whole of the tamper-evidence. Edited bytes
// essentially never decode to valid JSON carrying both the current version and
// this scope, and an HMAC would add a key and a token lifetime to manage
// without addressing a threat that matters here.
func decodeToken(token, scope string) (after string, ok bool) {
	// Length-gated before decoding, so rejecting an oversized token costs no
	// allocation at all. The bound is derived from the cursor's own maximum
	// rather than a round number: a scope long enough to make a legitimate token
	// exceed a hard-coded 2 KiB would turn this into a silent false reject.
	if len(token) > maxTokenLen(scope) {
		return "", false
	}

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
	// encoding/json leaves a missing "k" as a nil slice with no error, so
	// without this the craftable cursor {"v":1,"s":"leases"} would decode to an
	// empty After and silently restart the listing from row one.
	if len(decoded.After) == 0 {
		return "", false
	}

	if len(decoded.After) > MaxKeyBytes {
		return "", false
	}

	return string(decoded.After), true
}
