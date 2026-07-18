// Package auth provides service-to-service authentication for adapters: the
// bearer tokens weave presents, how they are minted, and how they are stored.
//
// The adapter never holds a usable token. Tokens are generated once, shown to
// the operator once, and persisted only as a hash — so a leaked token file is
// not a set of working credentials. That is the whole reason this package
// hashes rather than encrypts: unlike weave's credentialMgr, which must recover
// the plaintext to present a credential outbound, an adapter only ever needs to
// verify an inbound one.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// TokenPrefix marks adapter tokens so secret scanners can spot them and a
	// leak report can identify what was leaked.
	TokenPrefix = "wadapt_"

	// tokenBytes is the entropy behind a token. 32 bytes is far past any
	// brute-force concern, which is what lets the store use a plain hash.
	tokenBytes = 32

	// hashAlgorithm prefixes stored hashes so the algorithm can change later
	// without guessing at the format of existing entries.
	hashAlgorithm = "sha256"
)

// Generate returns a new bearer token. The caller must show it to the operator
// exactly once — nothing can recover it afterwards.
func Generate() (string, error) {
	buf := make([]byte, tokenBytes)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}

	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash returns the stored form of a token, e.g. "sha256:9f86d081…".
//
// A plain hash is correct here, not bcrypt/argon2: those exist to slow down
// brute force against low-entropy human passwords, whereas a token carries 256
// bits of entropy and has nothing to brute force. A KDF would only add latency
// to every request.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hashAlgorithm + ":" + hex.EncodeToString(sum[:])
}

// Mask renders a token for display, keeping only enough tail to recognise it.
// Used wherever a token would otherwise be echoed in full.
func Mask(token string) string {
	const visible = 4

	if len(token) <= visible {
		return strings.Repeat("*", len(token))
	}

	return "****" + token[len(token)-visible:]
}
