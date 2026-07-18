// Package etag implements conditional reads: a strong entity-tag over the
// response representation, and RFC 9110 If-None-Match evaluation.
//
// This is what makes weave's polling cheap. weave asks an adapter for the same
// collection on an interval; with an ETag it sends the tag back and gets a 304
// with no body whenever nothing changed, so the cost of a poll drops to a
// request line and a status.
//
// Tags are always derived from the **representation** — the exact bytes the
// response would carry — never from a backend-native version field. Backend
// versions are adapter-specific and would make the mechanism non-uniform, and a
// backend that reports "unchanged" while its serialization differs would hand
// clients a stale body under a fresh tag.
package etag

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// weakPrefix marks a weak entity-tag (RFC 9110 §8.8.3).
	weakPrefix = "W/"
	// wildcard matches any current representation.
	wildcard = "*"
)

// Compute returns the strong entity-tag for a representation, quoted per
// RFC 9110 §8.8.3.
//
// It hashes the serialized bytes rather than the value they came from. Go's
// encoding/json is deterministic for structs and sorts map keys, so hashing the
// value would usually agree — but "usually" is the wrong guarantee for a tag a
// client uses to decide whether to trust its cache. Hashing the output makes a
// tag that disagrees with its body impossible by construction.
func Compute(body []byte) string {
	sum := sha256.Sum256(body)

	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Matches reports whether an If-None-Match header value matches the given
// entity-tag, per RFC 9110 §13.1.2.
//
// If-None-Match uses the **weak** comparison function, so `W/"x"` matches
// `"x"`: the client is asking "is this still the same representation?", and a
// semantically equivalent one is a legitimate cache hit. (If-Match, on the
// write side in M3, uses strong comparison instead — the two are deliberately
// different and must not share this function.)
func Matches(ifNoneMatch, etag string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}

	// "*" matches any existing representation. Reaching here means one exists.
	if ifNoneMatch == wildcard {
		return true
	}

	want := opaqueTag(etag)
	if want == "" {
		return false
	}

	for _, candidate := range splitTags(ifNoneMatch) {
		if opaqueTag(candidate) == want {
			return true
		}
	}

	return false
}

// splitTags splits an If-None-Match list on the commas that separate tags,
// ignoring commas inside a quoted tag.
//
// RFC 9110's etagc is %x21 / %x23-7E, which includes the comma, so an
// entity-tag may legitimately contain one. A plain strings.Split would shred
// such a tag into two invalid fragments. Our own tags are hex and can never
// contain a comma, but the write side (If-Match) compares tags minted
// elsewhere, and this parser is what it will reach for.
func splitTags(header string) []string {
	var (
		tags     []string
		start    int
		inQuotes bool
	)

	for i := range len(header) {
		switch header[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				tags = append(tags, header[start:i])
				start = i + 1
			}
		}
	}

	return append(tags, header[start:])
}

// opaqueTag strips surrounding whitespace and any weak marker, returning the
// quoted opaque tag used for weak comparison. It returns "" for a value that is
// not a syntactically valid entity-tag, so garbage never compares equal to
// garbage.
func opaqueTag(value string) string {
	value = strings.TrimSpace(value)

	// The weak marker is defined case-sensitively (%s"W/"), but a client that
	// sends "w/" is asking a question we can answer, and accepting it cannot
	// produce a false match — the opaque tag still has to be equal. Rejecting
	// it would silently cost that client every 304 it should have received.
	if len(value) >= len(weakPrefix) && strings.EqualFold(value[:len(weakPrefix)], weakPrefix) {
		value = value[len(weakPrefix):]
	}

	if len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
		return ""
	}

	return value
}
