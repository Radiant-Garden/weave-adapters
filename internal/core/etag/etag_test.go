/*
Testing: etag.go

Pending:

Tested:
  Compute
    - TestCompute_ShouldBeStableForTheSameBytes: the same representation tags the same.
    - TestCompute_ShouldChangeWithTheRepresentation: any byte difference changes the tag.
    - TestCompute_ShouldReturnAQuotedTag: RFC 9110 syntax.
  Matches
    - TestMatches_ShouldEvaluateIfNoneMatch: table over wildcard, lists, weak forms, and junk.
    - FuzzMatches: attacker-controlled header never panics or hits on a tag it lacks.

Tested elsewhere:
  How a tag reaches the wire and drives a 304 is covered in handler_test.go.

Declined:
  opaqueTag — unexported parser, exercised through every Matches case including
  the malformed ones.

Additional Remarks:
  Compute's hash is not asserted against a fixed digest. Pinning one would test
  crypto/sha256 rather than this package, and would make swapping the algorithm
  a test edit rather than a decision. What matters to a client is that the tag
  is stable for identical bytes and differs otherwise, which is what is asserted.
*/

package etag

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompute_ShouldBeStableForTheSameBytes(t *testing.T) {
	t.Parallel()

	// ARRANGE — two distinct slices holding identical bytes, as two calls
	// serializing the same resource would produce.
	body := []byte(`{"items":[{"id":"a"}],"nextPageToken":""}`)
	sameBytes := append([]byte(nil), body...)

	// ACT
	first, second := Compute(body), Compute(sameBytes)

	// ASSERT — a client's cache is only valid if this holds.
	assert.Equal(t, first, second)
}

func TestCompute_ShouldChangeWithTheRepresentation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
	}{
		{name: "should differ on a changed value", a: `{"id":"a"}`, b: `{"id":"b"}`},
		{name: "should differ on added whitespace", a: `{"id":"a"}`, b: `{"id": "a"}`},
		{name: "should differ on field order", a: `{"a":1,"b":2}`, b: `{"b":2,"a":1}`},
		{name: "should differ on an empty body", a: `{}`, b: ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT — the tag covers the bytes, not the meaning: a
			// re-serialization that changes the bytes must change the tag,
			// because that is what the client compares.
			assert.NotEqual(t, Compute([]byte(tt.a)), Compute([]byte(tt.b)))
		})
	}
}

func TestCompute_ShouldReturnAQuotedTag(t *testing.T) {
	t.Parallel()

	// ACT
	tag := Compute([]byte("x"))

	// ASSERT — an unquoted tag is not a valid entity-tag and clients may drop it.
	assert.True(t, strings.HasPrefix(tag, `"`), "tag %q should be quoted", tag)
	assert.True(t, strings.HasSuffix(tag, `"`), "tag %q should be quoted", tag)
	assert.NotContains(t, strings.Trim(tag, `"`), `"`)
}

func TestMatches_ShouldEvaluateIfNoneMatch(t *testing.T) {
	t.Parallel()

	const tag = `"abc123"`

	tests := []struct {
		name         string
		ifNoneMatch  string
		want         bool
		whyItMatters string
	}{
		{
			name: "should match an identical tag", ifNoneMatch: `"abc123"`, want: true,
			whyItMatters: "the ordinary cache hit",
		},
		{
			name: "should match the wildcard", ifNoneMatch: "*", want: true,
			whyItMatters: "a client asking whether any representation exists",
		},
		{
			name: "should match a weak form of the same tag", ifNoneMatch: `W/"abc123"`, want: true,
			whyItMatters: "If-None-Match uses weak comparison, so W/ must still hit",
		},
		{
			name: "should match within a list", ifNoneMatch: `"other", W/"abc123", "more"`, want: true,
			whyItMatters: "clients may hold several cached representations",
		},
		{
			name: "should match despite surrounding whitespace", ifNoneMatch: "  \t" + `"abc123"` + "  ", want: true,
			whyItMatters: "header whitespace is not significant",
		},
		{
			name: "should not match a different tag", ifNoneMatch: `"different"`, want: false,
			whyItMatters: "the representation changed, so the body must be sent",
		},
		{
			name: "should not match an empty header", ifNoneMatch: "", want: false,
			whyItMatters: "no condition was asked",
		},
		{
			name: "should not match an unquoted value", ifNoneMatch: "abc123", want: false,
			whyItMatters: "not a valid entity-tag; treating it as one would 304 on malformed input",
		},
		{
			name: "should not match a prefix of the tag", ifNoneMatch: `"abc"`, want: false,
			whyItMatters: "a truncated tag must never satisfy the condition",
		},
		{
			name: "should not match a bare quote", ifNoneMatch: `"`, want: false,
			whyItMatters: "malformed input must not compare equal to anything",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tt.want, Matches(tt.ifNoneMatch, tag), tt.whyItMatters)
		})
	}
}

// FuzzMatches drives the If-None-Match parser with arbitrary header bytes. The
// value is attacker-controlled, so it must never panic — and must never report
// a match for a tag it does not contain, which would hand a client a 304 for a
// representation it has never seen.
func FuzzMatches(f *testing.F) {
	const tag = `"abc123"`

	for _, seed := range []string{"", "*", tag, "W/" + tag, `"a", "b"`, `"`, "W/", ",,,", "abc123"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, ifNoneMatch string) {
		if !Matches(ifNoneMatch, tag) {
			return
		}

		// A match is only legitimate for the wildcard or for input that
		// actually carries the tag.
		if strings.TrimSpace(ifNoneMatch) == wildcard || strings.Contains(ifNoneMatch, `abc123`) {
			return
		}

		t.Fatalf("Matches(%q) reported a hit without containing the tag", ifNoneMatch)
	})
}
