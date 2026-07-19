package dhcpwindows

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// wadaptIDBytes is how much HMAC output the ID carries: 8 bytes, 64 bits.
//
// Eight *characters* would be 40 bits — a 24-bit cut, and an easy misreading to
// ship. Eight bytes encode to exactly 13 base32 characters, unpadded, which is
// what weave's keyShape regexp pins.
const wadaptIDBytes = 8

// WadaptIDLength is the character length of every derived ID. It is fixed: 8
// bytes always encode to 13 base32 characters.
const WadaptIDLength = 13

// wadaptIDEncoding is RFC 4648 §7 base32hex, lowercase, unpadded.
//
// The extended-hex alphabet rather than the standard one, and the difference is
// not cosmetic. In the standard alphabet (abcdefghijklmnopqrstuvwxyz234567)
// values 26–31 encode to '2'–'7', which sort *before* the letters in ASCII, so
// lexicographic order of the encoded string is not the order of the underlying
// bytes. base32hex was designed so that sort order survives encoding.
//
// Pagination sorts and resumes on the encoded string, so consistency holds
// whatever the alphabet — but this makes encoded order equal byte order too, so
// the two cannot diverge even if a later change compares the wrong one. That is
// defence in depth, not the primary guarantee.
//
// The alphabet is contract-visible and unchangeable: weave's keyShape regexp
// encodes it, and an exposed ID never changes.
var wadaptIDEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// idSeparator keeps the two derivation inputs from running together. Without
// it ("dhcp1", "0.0.0.0") and ("dhcp", "10.0.0.0") hash identical input.
const idSeparator = 0x00

// deriveWadaptID computes a scope's stable identity:
//
//	base32hex(HMAC-SHA256(namespaceKey, serverName ‖ 0x00 ‖ scopeID))[:8 bytes]
//
// Nothing is written to the backend. The ID is a pure function of data every
// scope already has, so there is no unmarked state, no seeding phase, and no
// failure mode where a scope is servable but identity-less.
//
// It is stable exactly as long as its three inputs are. namespaceKey is
// provisioned config that is never auto-generated, serverName is provisioned
// rather than read from the environment, and scopeID cannot change on a live
// scope — which is what makes the derivation an identity rather than a hash.
func deriveWadaptID(namespaceKey []byte, serverName, scopeID string) string {
	mac := hmac.New(sha256.New, namespaceKey)

	// Hash.Write never returns an error (documented on hash.Hash), so the
	// results are discarded rather than checked into an unreachable branch.
	mac.Write([]byte(serverName))
	mac.Write([]byte{idSeparator})
	mac.Write([]byte(scopeID))

	return wadaptIDEncoding.EncodeToString(mac.Sum(nil)[:wadaptIDBytes])
}

// canonicalServerName normalizes the provisioned server identity before it is
// hashed: lowercased, trailing dot stripped, surrounding space removed.
//
// This runs once at load rather than per derivation, so that "DHCP01",
// "dhcp01" and "dhcp01." are one identity rather than three. Without it, an
// operator correcting the case of a config value would re-key the whole fleet —
// the drift risk the plan mitigates by requiring the key to be provisioned in
// the first place.
func canonicalServerName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
