/*
Testing: jsonshape.go

Pending:

Tested:

	Of
	  - TestOf_ShouldDescribeTaggedFields: name, omitempty and kind come off the json tag.
	  - TestOf_ShouldPromoteEmbeddedStructFields: an embedded struct's fields belong to the parent, whether the embedded type is exported or not.
	  - TestOf_ShouldFallBackToTheGoNameWhenUntagged: an exported field with no tag still serializes.
	  - TestOf_ShouldIgnoreUnexportedAndSkippedFields: unexported fields and `json:"-"` never reach the wire.
	  - TestOf_ShouldTreatPointersAndNamedTypesAsTheirUnderlyingKind: differences a client cannot observe are not differences.
	  - TestOf_ShouldAgreeWithEncodingJSON: the described field set is the one encoding/json actually emits.
	Names
	  - TestNames_ShouldReturnSortedNames: two field sets compare by content, not by map order.

Tested elsewhere:

	Its real job — holding a hand-written type and its generated counterpart to
	one wire format — is exercised by api/common and api/dhcp-windows, which are
	the reason this package exists.

Declined:

	Asserting the require failures (duplicate JSON name, non-struct argument).
	They take a *testing.T and fail the caller's test, so provoking one means
	either a fake TB or a subprocess; the messages are diagnostics for a broken
	test rather than behaviour a caller depends on.

Additional Remarks:

	TestOf_ShouldAgreeWithEncodingJSON is the load-bearing one. Every other test
	asserts what this package *says* about a struct; that one checks the claim
	against the encoder whose behaviour it is modelling, which is the only way a
	wrong model shows up as a failure rather than as a confident wrong answer.

	The embedded-struct tests exist because a copy of this helper made for the
	adapter spec dropped that branch and nothing caught it — both sides of a
	comparison collapse an embedded struct to one entry and compare equal while
	the wire formats differ. Writing the encoding/json cross-check then found a
	second, older bug in the branch itself: it sat behind an IsExported check,
	so an embedded *unexported* struct type was skipped even though json
	promotes through one. Both are fixed; the ordering is load-bearing.
*/
package jsonshape

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagged covers the ordinary cases: a required field, an omitempty one, a
// renamed one, and a slice.
type tagged struct {
	Name     string   `json:"name"`
	Detail   string   `json:"detail,omitempty"`
	Renamed  string   `json:"wireName"`
	Statuses []string `json:"statuses"`
}

// The embedding cases encoding/json handles specially. Both promote their
// fields onto the parent's wire format, and the unexported one is the trap:
// reflect names an embedded field after its type, so embedding an unexported
// type yields an unexported field that json still promotes through.
type unexportedBase struct {
	ID string `json:"id"`
}

// ExportedBase is exported so the two embedding cases differ only in that.
type ExportedBase struct {
	Kind string `json:"kind"`
}

type embedsUnexported struct {
	unexportedBase

	Extra string `json:"extra"`
}

type embedsExported struct {
	ExportedBase

	Extra string `json:"extra"`
}

func TestOf_ShouldDescribeTaggedFields(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	fields := Of(t, tagged{})

	// ASSERT
	assert.Equal(t, map[string]Field{
		"name":     {Name: "name", Kind: "string"},
		"detail":   {Name: "detail", OmitEmpty: true, Kind: "string"},
		"wireName": {Name: "wireName", Kind: "string"},
		"statuses": {Name: "statuses", Kind: "[]string"},
	}, fields)
}

func TestOf_ShouldPromoteEmbeddedStructFields(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT — the promoted field belongs to the parent's field
	// set, not to a nested entry named after the embedded type. Without the
	// promotion branch, an embedding type and a non-embedding one both reduce to
	// a single struct-kinded field and compare equal while their wire formats
	// differ — the drift this package exists to catch.
	assert.Equal(t, map[string]Field{
		"kind":  {Name: "kind", Kind: "string"},
		"extra": {Name: "extra", Kind: "string"},
	}, Of(t, embedsExported{}))

	// The unexported case, which is the one that actually went wrong. reflect
	// reports the embedded field under its type name, so it reads as unexported
	// and an IsExported check placed before the promotion branch silently drops
	// id — even though encoding/json emits it. Both sides of a conformance
	// comparison would drop it identically and still compare equal.
	assert.Equal(t, map[string]Field{
		"id":    {Name: "id", Kind: "string"},
		"extra": {Name: "extra", Kind: "string"},
	}, Of(t, embedsUnexported{}))
}

func TestOf_ShouldFallBackToTheGoNameWhenUntagged(t *testing.T) {
	t.Parallel()

	// ARRANGE — an exported field with no json tag still serializes, under its
	// Go name. A scan that only read tags would miss it entirely and report a
	// field set the encoder does not produce.
	type untagged struct {
		Tagged string `json:"tagged"`
		Bare   string
	}

	// ACT
	fields := Of(t, untagged{})

	// ASSERT
	assert.Equal(t, map[string]Field{
		"tagged": {Name: "tagged", Kind: "string"},
		"Bare":   {Name: "Bare", Kind: "string"},
	}, fields)
}

func TestOf_ShouldIgnoreUnexportedAndSkippedFields(t *testing.T) {
	t.Parallel()

	// ARRANGE
	type mixed struct {
		Visible string `json:"visible"`
		Skipped string `json:"-"`
		hidden  string //nolint:unused // present precisely to be ignored
	}

	// ACT
	fields := Of(t, mixed{})

	// ASSERT — neither reaches a client, so neither is part of the contract.
	assert.Equal(t, map[string]Field{"visible": {Name: "visible", Kind: "string"}}, fields)
}

func TestOf_ShouldTreatPointersAndNamedTypesAsTheirUnderlyingKind(t *testing.T) {
	t.Parallel()

	// ARRANGE — a pointer is transparent on the wire and a named string type is
	// not observable at all, so neither may count as a difference. This is what
	// lets apierror.FieldError and the generated common.FieldError compare
	// equal despite being different Go types.
	type status string

	type shapes struct {
		Pointer *string  `json:"pointer"`
		Named   status   `json:"named"`
		Slice   []status `json:"slice"`
	}

	// ACT
	fields := Of(t, shapes{})

	// ASSERT
	assert.Equal(t, "string", fields["pointer"].Kind)
	assert.Equal(t, "string", fields["named"].Kind)
	assert.Equal(t, "[]string", fields["slice"].Kind)
}

func TestOf_ShouldAgreeWithEncodingJSON(t *testing.T) {
	t.Parallel()

	// ARRANGE — this package models encoding/json's field resolution, so the
	// model is checked against the encoder rather than against itself. Every
	// other test here would pass just as happily against a wrong model.
	for _, tt := range []struct {
		name  string
		value any
	}{
		{name: "tagged fields", value: tagged{Name: "n", Detail: "d", Renamed: "r", Statuses: []string{"s"}}},
		{name: "embedded exported struct", value: embedsExported{ExportedBase{Kind: "k"}, "e"}},
		{name: "embedded unexported struct", value: embedsUnexported{unexportedBase{ID: "i"}, "e"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT — every field populated, so nothing is dropped by omitempty
			// and the encoder emits the full set.
			encoded, err := json.Marshal(tt.value)
			require.NoError(t, err)

			var emitted map[string]any
			require.NoError(t, json.Unmarshal(encoded, &emitted))

			// ASSERT — the names this package reports are the names the encoder
			// actually writes.
			described := make([]string, 0, len(emitted))
			for name := range emitted {
				described = append(described, name)
			}

			assert.ElementsMatch(t, described, Names(Of(t, tt.value)))
		})
	}
}

func TestNames_ShouldReturnSortedNames(t *testing.T) {
	t.Parallel()

	// ARRANGE — map iteration order is random, so an unsorted result would make
	// assert.Equal on two field-name lists flake rather than fail.
	fields := Of(t, tagged{})

	// ACT / ASSERT
	assert.Equal(t, []string{"detail", "name", "statuses", "wireName"}, Names(fields))
}
