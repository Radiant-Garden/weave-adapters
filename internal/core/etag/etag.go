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

	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		if opaqueTag(candidate) == want {
			return true
		}
	}

	return false
}

// opaqueTag strips surrounding whitespace and any weak marker, returning the
// quoted opaque tag used for weak comparison. It returns "" for a value that is
// not a syntactically valid entity-tag, so garbage never compares equal to
// garbage.
func opaqueTag(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, weakPrefix)

	if len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
		return ""
	}

	return value
}
