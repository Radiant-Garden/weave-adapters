/*
Testing: token.go

Pending:

Tested:
  encodeToken / decodeToken
    - TestToken_ShouldRoundTripTheResumeKey: what was encoded is what comes back, incl. non-UTF-8 and the size cap.
    - TestToken_ShouldProduceAQuerySafeToken: no escaping and no padding on the wire.
    - TestDecodeToken_ShouldRejectAnUnreadableToken: table over junk, truncation, oversized keys, and hand-built cursors.
    - TestDecodeToken_ShouldRejectAForeignScope: a token minted by another collection is not readable here.
    - TestDecodeToken_ShouldRejectAnOlderVersion: a wire-format bump invalidates outstanding tokens.
    - FuzzDecodeToken: an attacker-controlled token never panics, and an accepted one carries a bounded key.
    - FuzzEncodeToken: any key round-trips byte for byte, or is refused for exceeding MaxKeyBytes.

Tested elsewhere:
  That an unreadable token becomes a 400 problem+json rather than a panic or a
  full scan is asserted in page_test.go, which owns the Parse boundary.

Declined:

Additional Remarks:
  The encoded form is deliberately asserted as decodable-by-anyone (the
  round-trip and query-safety tests read the base64 back). That is the documented
  contract: tokens are opaque to clients by agreement, not by encryption. A test
  asserting the bytes were unreadable would be asserting a property this package
  does not claim and does not need — see the package comment on why forgery is
  not the threat model.
*/

package pagination

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToken_ShouldRoundTripTheResumeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{name: "should round-trip an ordinary key", key: "lease-0042"},
		{name: "should round-trip a key with URL-unsafe bytes", key: "10.0.0.1/24 & more?=x"},
		{name: "should round-trip a key with non-ASCII", key: "clé-résumé-日本"},
		// json.Marshal does not reject invalid UTF-8, it substitutes U+FFFD.
		// A string cursor field would return a different key here, with ok=true
		// and no error anywhere — the handler would resume from the wrong row.
		{name: "should round-trip a key that is not valid UTF-8", key: "lease-\xff\xfe-tail"},
		{name: "should round-trip a key at the size cap", key: strings.Repeat("k", MaxKeyBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			token := encodeToken("leases", tt.key)
			got, ok := decodeToken(token, "leases")

			// ASSERT
			require.True(t, ok)
			assert.Equal(t, tt.key, got)
		})
	}
}

func TestToken_ShouldProduceAQuerySafeToken(t *testing.T) {
	t.Parallel()

	// ARRANGE — a key full of bytes that would need escaping if they reached
	// the query string unencoded.
	token := encodeToken("leases", "a/b+c=d?e&f")

	// ACT — the token as a client would send it back.
	escaped := url.QueryEscape(token)

	// ASSERT — surviving QueryEscape unchanged is what lets a client echo the
	// token back without an encoding step we would have to specify.
	assert.Equal(t, token, escaped)
	assert.NotContains(t, token, "=", "padding would be trimmed by a client and break the decode")
}

func TestDecodeToken_ShouldRejectAnUnreadableToken(t *testing.T) {
	t.Parallel()

	// Cursors that decode as JSON and carry the right version and scope, but
	// whose key is unusable — proof the checks reach the field's value and do
	// not stop at "is it JSON".
	//
	// A JSON array is deliberately NOT one of these: encoding/json accepts
	// [1,2] into a []byte as the bytes 0x01 0x02, so it is a legitimate
	// alternate encoding of a key rather than a malformed one.
	wrongType, err := json.Marshal(map[string]any{"v": tokenVersion, "s": "leases", "k": 123})
	require.NoError(t, err)

	nullKey, err := json.Marshal(map[string]any{"v": tokenVersion, "s": "leases", "k": nil})
	require.NoError(t, err)

	noKey, err := json.Marshal(map[string]any{"v": tokenVersion, "s": "leases"})
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
	}{
		{name: "should reject junk", token: "not-a-token"},
		{name: "should reject standard-alphabet base64", token: base64.StdEncoding.EncodeToString([]byte(`{"v":1,"s":"leases","k":"x"}`))},
		{name: "should reject valid base64 that is not JSON", token: base64.RawURLEncoding.EncodeToString([]byte("plain text"))},
		{name: "should reject JSON that is not a cursor", token: base64.RawURLEncoding.EncodeToString([]byte(`[1,2,3]`))},
		{name: "should reject a cursor whose key has the wrong type", token: base64.RawURLEncoding.EncodeToString(wrongType)},
		{name: "should reject a cursor whose key is not base64", token: base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":"leases","k":"!!!"}`))},
		{name: "should reject a truncated token", token: encodeToken("leases", "lease-0042")[:8]},
		{name: "should reject an empty token", token: ""},
		{name: "should reject a key past the size cap", token: encodeToken("leases", strings.Repeat("k", MaxKeyBytes+1))},
		// The silent-full-scan cases: a well-formed cursor carrying no key at
		// all would otherwise decode to "" and restart the listing from row one.
		{name: "should reject a cursor with a null key", token: base64.RawURLEncoding.EncodeToString(nullKey)},
		{name: "should reject a cursor with no key at all", token: base64.RawURLEncoding.EncodeToString(noKey)},
		{name: "should reject a cursor with an empty key", token: encodeToken("leases", "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			got, ok := decodeToken(tt.token, "leases")

			// ASSERT — a bad token yields nothing at all. Returning a partial
			// key here is what would become a silent scan from the wrong place.
			assert.False(t, ok)
			assert.Empty(t, got)
		})
	}
}

func TestDecodeToken_ShouldRejectAForeignScope(t *testing.T) {
	t.Parallel()

	// ARRANGE — a perfectly valid token, minted by a different collection.
	token := encodeToken("scopes", "scope-7")

	// ACT
	got, ok := decodeToken(token, "leases")

	// ASSERT — without this the key would be applied to the wrong listing and
	// silently return a page from nowhere.
	assert.False(t, ok)
	assert.Empty(t, got)
}

func TestDecodeToken_ShouldRejectAnOlderVersion(t *testing.T) {
	t.Parallel()

	// ARRANGE — a cursor in a wire format this build no longer speaks.
	raw, err := json.Marshal(cursor{Version: tokenVersion - 1, Scope: "leases", After: []byte("lease-0042")})
	require.NoError(t, err)

	// ACT
	got, ok := decodeToken(base64.RawURLEncoding.EncodeToString(raw), "leases")

	// ASSERT — the point of the version field: a client mid-listing across a
	// deploy restarts cleanly instead of resuming against changed semantics.
	assert.False(t, ok)
	assert.Empty(t, got)
}

func FuzzDecodeToken(f *testing.F) {
	// The token is attacker-controlled: it arrives verbatim in a query string.
	f.Add(encodeToken("leases", "lease-0042"))
	f.Add(encodeToken("scopes", ""))
	f.Add("")
	f.Add("////")
	f.Add(strings.Repeat("A", 4096))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":"leases"}`)))

	f.Fuzz(func(t *testing.T, token string) {
		after, ok := decodeToken(token, "leases")

		// A rejected token must yield no key — anything else would let crafted
		// input steer a listing.
		if !ok {
			assert.Empty(t, after)

			return
		}

		// An accepted key is bounded, so a forged token cannot hand a handler
		// an arbitrarily large "identifier".
		assert.LessOrEqual(t, len(after), MaxKeyBytes)
	})
}

func FuzzEncodeToken(f *testing.F) {
	// Fidelity, fuzzed over the *key* rather than the token. Round-tripping a
	// key that came out of decodeToken would prove nothing: it has already been
	// through the JSON decoder, so it is valid UTF-8 by then and re-encoding it
	// is idempotent whatever the codec does to raw bytes.
	f.Add("lease-0042")
	f.Add("")
	f.Add("10.0.0.1/24 & more?=x")
	f.Add("clé-résumé-日本")
	f.Add("\xff\xfe")
	f.Add(strings.Repeat("k", MaxKeyBytes))

	f.Fuzz(func(t *testing.T, key string) {
		after, ok := decodeToken(encodeToken("leases", key), "leases")

		// An empty key is refused rather than read as "first page", and a key
		// over the cap is refused outright — refusing beats resuming from a
		// truncated key that still looks valid.
		if key == "" || len(key) > MaxKeyBytes {
			assert.False(t, ok)
			assert.Empty(t, after)

			return
		}

		// Below it, what went in must come back byte for byte. A key is a
		// position in someone's listing: a codec that "mostly" round-trips
		// silently skips or repeats rows.
		require.True(t, ok)
		assert.Equal(t, key, after)
	})
}
