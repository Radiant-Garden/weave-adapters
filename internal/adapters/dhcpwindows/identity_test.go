/*
Testing: identity.go

Pending:

Tested:

	deriveWadaptID
	  - TestDeriveWadaptID_ShouldBeStableAcrossRuns: the same three inputs yield the
	    same ID — the exit criterion that a host read twice across a restart is
	    identical. Derivation that is not stable is not an identity.
	  - TestDeriveWadaptID_ShouldChangeWhenAnyInputChanges: each of the three inputs
	    is load bearing, including the separator case.
	  - TestDeriveWadaptID_ShouldSeparateItsInputs: ("dhcp1","0.0.0.0") and
	    ("dhcp","10.0.0.0") must not hash identical input.
	  - TestDeriveWadaptID_ShouldEncodeThirteenBase32HexCharacters: the shape weave's
	    keyShape regexp pins, and the 8-bytes-not-8-characters distinction.
	  - TestDeriveWadaptID_ShouldPreserveByteOrderInEncodedOrder: the base32hex
	    property the standard alphabet does not have.

	NamespaceKeyFingerprint
	  - TestNamespaceKeyFingerprint_ShouldNeverRevealTheKey: it goes into a log line
	    at every startup, so it must be one-way rather than a shortened copy.
	  - TestNamespaceKeyFingerprint_ShouldIdentifyAKeyAcrossRestarts: stable for one
	    key, different for another — otherwise it cries re-key every restart, or
	    stays silent through a real one.
	  - TestNamespaceKeyFingerprint_ShouldReturnNothingForNoKey: an absent key is a
	    startup failure, not a value to fingerprint.

	canonicalServerName
	  - TestCanonicalServerName_ShouldFoldTheFormsOfOneName: case, trailing dots and
	    surrounding space are one identity, not several.

Tested elsewhere:

	Derivation applied across a whole list, and collision detection: client_test.go.

Declined:

	Testing HMAC-SHA256 itself: it is crypto/hmac's, and we do not test what we do
	  not own. What is tested here is the composition — input framing, truncation
	  width, and alphabet — which is ours and is contract-visible.
	Property-based testing of the encoding: the alphabet is fixed and 13 characters
	  wide, so the interesting cases are enumerable and enumerated.

Additional Remarks:

	The expected IDs here are pinned as literals on purpose. They are computed by
	  the code under test, so on their own they would be a tautology — their value
	  is as a change detector: the ID is contract-visible and an exposed ID must
	  never change, so any edit to the framing, truncation or alphabet has to fail
	  these tests loudly rather than silently re-key a fleet. If one of these
	  literals ever needs updating, that is the signal to stop.
*/
package dhcpwindows

import (
	"encoding/base32"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fixed derivation inputs, so the pinned IDs below are reproducible.
const (
	fixedKey    = "namespace-key-for-tests-0123456789"
	fixedServer = "dhcp01.example.test"
	fixedScope  = "192.168.178.0"
)

func TestDeriveWadaptID_ShouldBeStableAcrossRuns(t *testing.T) {
	t.Parallel()

	// ACT — the same inputs, derived twice, as a restart would.
	first := deriveWadaptID([]byte(fixedKey), fixedServer, fixedScope)
	second := deriveWadaptID([]byte(fixedKey), fixedServer, fixedScope)

	// ASSERT — the exit criterion: the same host read twice across a restart
	// yields identical IDs. There is no state to persist precisely because this
	// holds.
	assert.Equal(t, first, second)

	// A pinned literal, so a change to the framing, truncation or alphabet
	// fails here rather than silently re-keying every deployment.
	//
	// Cross-checked against an independent implementation of the specified
	// derivation rather than simply recorded from this code — otherwise the pin
	// would only prove the implementation is unchanged, not that it is right:
	//
	//	hmac.new(key, server + b"\x00" + scope, sha256).digest()[:8]
	//	-> 5781b34bb655738c -> b32hexencode -> "au0r6itmalpoo"
	assert.Equal(t, "au0r6itmalpoo", first)
}

func TestDeriveWadaptID_ShouldChangeWhenAnyInputChanges(t *testing.T) {
	t.Parallel()

	base := deriveWadaptID([]byte(fixedKey), fixedServer, fixedScope)

	tests := []struct {
		name   string
		key    string
		server string
		scope  string
	}{
		{name: "a different namespace key", key: "another-key-0123456789", server: fixedServer, scope: fixedScope},
		{name: "a different server", key: fixedKey, server: "dhcp02.example.test", scope: fixedScope},
		{name: "a different scope", key: fixedKey, server: fixedServer, scope: "192.168.179.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			got := deriveWadaptID([]byte(tc.key), tc.server, tc.scope)

			// ASSERT — all three inputs are load bearing. The namespace key in
			// particular is what makes two sites both running DHCP01 distinct.
			assert.NotEqual(t, base, got)
		})
	}
}

func TestDeriveWadaptID_ShouldSeparateItsInputs(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — concatenated without a separator these are both
	// "dhcp10.0.0.0".
	run := deriveWadaptID([]byte(fixedKey), "dhcp1", "0.0.0.0")
	together := deriveWadaptID([]byte(fixedKey), "dhcp", "10.0.0.0")

	// ASSERT — the 0x00 separator is not decorative: without it these two
	// distinct scopes on two distinct servers share an identity.
	assert.NotEqual(t, run, together)
}

func TestDeriveWadaptID_ShouldEncodeThirteenBase32HexCharacters(t *testing.T) {
	t.Parallel()

	tests := []string{
		"192.168.178.0",
		"10.0.5.0",
		"172.16.0.0",
		"0.0.0.0",
		"255.255.255.255",
	}

	for _, scope := range tests {
		t.Run(scope, func(t *testing.T) {
			t.Parallel()

			// ACT
			got := deriveWadaptID([]byte(fixedKey), fixedServer, scope)

			// ASSERT — 8 bytes of HMAC output, not 8 characters: 8 characters
			// would be 40 bits, a 24-bit cut, and an easy misreading to ship.
			// 8 bytes encode to exactly 13 characters, which is what weave's
			// keyShape regexp ^[0-9a-v]{13}$ pins.
			assert.Len(t, got, WadaptIDLength)
			assert.Regexp(t, `^[0-9a-v]{13}$`, got)
		})
	}
}

func TestDeriveWadaptID_ShouldPreserveByteOrderInEncodedOrder(t *testing.T) {
	t.Parallel()

	// ARRANGE — the exact counterexample that ruled out the standard alphabet.
	// Ascending bytes, whose standard-alphabet encodings descend: values 26-31
	// encode to '2'-'7', which sort before the letters in ASCII.
	low := []byte{0x00, 0, 0, 0, 0, 0, 0, 0}
	high := []byte{0xd0, 0, 0, 0, 0, 0, 0, 0}

	standard := base32.StdEncoding.WithPadding(base32.NoPadding)

	// ACT
	standardLow, standardHigh := standard.EncodeToString(low), standard.EncodeToString(high)
	hexLow, hexHigh := wadaptIDEncoding.EncodeToString(low), wadaptIDEncoding.EncodeToString(high)

	// ASSERT — the standard alphabet inverts the order, base32hex preserves it.
	// Pagination sorts and resumes on the encoded string, so consistency holds
	// either way; this makes encoded order equal byte order too, so the two
	// cannot diverge even if a later change compares the wrong one.
	assert.Greater(t, standardLow, standardHigh, "the standard alphabet is expected to invert this order")
	assert.Less(t, hexLow, hexHigh, "base32hex must preserve byte order in encoded order")
}

func TestNamespaceKeyFingerprint_ShouldNeverRevealTheKey(t *testing.T) {
	t.Parallel()

	// ARRANGE — a key with a recognisable substring, so leakage is detectable
	// rather than merely improbable.
	const key = "supersecret-namespace-key-0123456789"

	// ACT
	got := NamespaceKeyFingerprint(key)

	// ASSERT — this value goes into a log line at every startup, which gets
	// shipped, pasted into tickets and indexed by a SIEM. The key is
	// backup-critical, so the fingerprint must be a one-way function of it and
	// not merely a shortened copy.
	assert.NotContains(t, got, "supersecret")
	assert.NotContains(t, got, key)
	assert.NotContains(t, key, got)
	assert.Len(t, got, fingerprintBytes*2, "a fingerprint is %d bytes of hex", fingerprintBytes)
	assert.Regexp(t, `^[0-9a-f]+$`, got)
}

func TestNamespaceKeyFingerprint_ShouldIdentifyAKeyAcrossRestarts(t *testing.T) {
	t.Parallel()

	// ACT
	first := NamespaceKeyFingerprint(fixedKey)
	second := NamespaceKeyFingerprint(fixedKey)
	other := NamespaceKeyFingerprint(fixedKey + "-rotated")

	// ASSERT — the whole point is comparing one startup against the previous
	// one, so it has to be stable for a key and different for another. A
	// fingerprint that drifted would cry re-key on every restart; one that
	// collided would stay silent through a real one.
	assert.Equal(t, first, second)
	assert.NotEqual(t, first, other)
}

func TestNamespaceKeyFingerprint_ShouldReturnNothingForNoKey(t *testing.T) {
	t.Parallel()

	// ACT / ASSERT — an absent key is a startup failure, not a value to
	// fingerprint. Hashing "" would put a fixed, real-looking fingerprint in the
	// log for a configuration that cannot run, and that constant would be
	// identical across every broken deployment.
	assert.Empty(t, NamespaceKeyFingerprint(""))
}

func TestCanonicalServerName_ShouldFoldTheFormsOfOneName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "already canonical", in: "dhcp01.example.test", want: "dhcp01.example.test"},
		{name: "uppercase", in: "DHCP01.EXAMPLE.TEST", want: "dhcp01.example.test"},
		{name: "mixed case", in: "Dhcp01.Example.Test", want: "dhcp01.example.test"},
		{name: "trailing dot", in: "dhcp01.example.test.", want: "dhcp01.example.test"},
		// TrimSuffix would leave this as a second identity for the same host.
		{name: "several trailing dots", in: "dhcp01.example.test..", want: "dhcp01.example.test"},
		{name: "surrounding space", in: "  dhcp01.example.test  ", want: "dhcp01.example.test"},
		{name: "all at once", in: "  DHCP01.Example.TEST.  ", want: "dhcp01.example.test"},
		{name: "empty stays empty", in: "", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			got := canonicalServerName(tc.in)

			// ASSERT — without this, an operator correcting the case of a
			// config value would re-derive every ID in the deployment.
			assert.Equal(t, tc.want, got)
		})
	}
}
