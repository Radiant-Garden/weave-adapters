/*
Testing: common.go

Pending:

Tested:
  errors.yaml -> errors.gen.go (every catalog linked via internal/catalogs, so
  adapter-owned response codes count as live)
    - TestProblem_ShouldMatchTheHandWrittenStruct: generated Problem and apierror.Problem carry the same JSON fields.
    - TestProblem_ShouldRoundTripThroughTheHandWrittenStruct: a populated problem survives both types byte-identically.
    - TestFieldError_ShouldMatchTheHandWrittenStruct: same for the errors[] element.
    - TestProblemType_ShouldMatchTheLiveTaxonomy: the enum and the Go taxonomy list exactly the same codes.
  pagination.yaml -> pagination.gen.go
    - TestPageEnvelope_ShouldMatchTheHandWrittenStruct: generated envelope and pagination.Page agree on field names, and on optionality on both sides.
    - TestPageParameters_ShouldMatchTheHandWrittenNames: the spec's query parameters are the constants handlers actually read.
  jobs.yaml -> jobs.gen.go
    - TestJob_ShouldOmitAbsentOptionalFields: a pending job renders exactly id/status/createdAt — no zero timestamp, no empty error object.

Tested elsewhere:
  The behaviour behind these shapes is tested in the packages that own it —
  internal/core/apierror and internal/core/pagination. Nothing here exercises
  logic; these assert that two descriptions of one wire format agree.

Declined:
  The *.gen.go files get no test files of their own. They are generated, and a
  per-file test would be bookkeeping rather than signal; what is worth asserting
  about them is that they match the hand-written types, which is this file.

Additional Remarks:
  Field types are compared by JSON name, optionality and kind rather than by
  reflect.Type. The two sides legitimately use different named types for the
  same wire shape (apierror.FieldError vs common.FieldError), so requiring
  identical Go types would fail on a difference clients cannot observe.
*/

package common

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	// The enum below must list every code some event emits, and events register
	// from init() — so this test can only be right if every catalog is linked.
	// Without it the adapter's backend codes look like ghost entries.
	_ "github.com/radiantgarden/weave-adapters/internal/catalogs"
	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/jsonshape"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

func TestProblem_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	generated := jsonshape.Of(t, Problem{})
	handWritten := jsonshape.Of(t, apierror.Problem{})

	// ASSERT — two descriptions of one wire format; a client cannot tell which
	// produced a response, so they must not differ.
	assert.Equal(t, handWritten, generated)
}

func TestProblem_ShouldRoundTripThroughTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE — every field populated, including the extensions, so an omission
	// on either side shows up as a difference rather than as a shared zero.
	original := apierror.Problem{
		Type:         apierror.TypeFor(events.CodeValidationFailed),
		Title:        "Validation failed",
		Status:       400,
		Detail:       "The request has invalid parameters.",
		Instance:     "/api/v1/leases",
		RequestID:    "9f1c2d3e4a5b6c7d",
		BackendError: "scope already exists",
		Errors: []apierror.FieldError{
			{Field: "pageSize", Message: "must be at least 1"},
		},
	}

	fromHandWritten, err := json.Marshal(original)
	require.NoError(t, err)

	// ACT — decode into the generated type and re-encode.
	var viaGenerated Problem
	require.NoError(t, json.Unmarshal(fromHandWritten, &viaGenerated))

	fromGenerated, err := json.Marshal(viaGenerated)
	require.NoError(t, err)

	// ASSERT — a field the generated type lacks would be dropped here, and a
	// renamed tag would move it.
	assert.JSONEq(t, string(fromHandWritten), string(fromGenerated))
}

func TestFieldError_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Equal(t, jsonshape.Of(t, apierror.FieldError{}), jsonshape.Of(t, FieldError{}))
}

func TestProblemType_ShouldMatchTheLiveTaxonomy(t *testing.T) {
	t.Parallel()

	// ARRANGE — the codes the running adapter can actually return.
	live := make(map[string]bool)

	for _, spec := range events.GetAll() {
		if spec.ResponseCode != "" {
			live[apierror.TypeFor(spec.ResponseCode)] = true
		}
	}

	require.NotEmpty(t, live, "the catalog should be registered via init")

	// ACT — the enum as the spec declares it, read from the YAML rather than
	// from the generated constants, so an entry added to one and not the other
	// is still caught.
	declared := problemTypeEnum(t)

	// ASSERT — a code the taxonomy gained but the spec never listed leaves
	// clients unable to recognise an error we return...
	for problemType := range live {
		assert.Contains(t, declared, problemType, "taxonomy code missing from errors.yaml")
	}

	// ...and one the spec lists that nothing emits is a ghost entry, the same
	// thing the event catalog forbids.
	for _, problemType := range declared {
		assert.True(t, live[problemType], "errors.yaml declares %q but no event emits it", problemType)
	}
}

// problemTypeEnum reads the ProblemType enum straight out of errors.yaml.
func problemTypeEnum(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile("errors.yaml")
	require.NoError(t, err)

	var doc struct {
		Components struct {
			Schemas struct {
				ProblemType struct {
					Enum []string `yaml:"enum"`
				} `yaml:"ProblemType"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}

	require.NoError(t, yaml.Unmarshal(raw, &doc))

	enum := doc.Components.Schemas.ProblemType.Enum
	require.NotEmpty(t, enum, "ProblemType enum should not be empty")

	return enum
}

func TestPageEnvelope_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE — the envelope is generic in Go and untyped in the spec, so the
	// item type differs by construction; the contract is the field set.
	generated := jsonshape.Of(t, PageEnvelope{})
	handWritten := jsonshape.Of(t, pagination.Page[struct{}]{})

	// ACT / ASSERT — the same field names on both sides.
	assert.ElementsMatch(t, jsonshape.Names(handWritten), jsonshape.Names(generated))

	// Optionality is asserted on BOTH sides. Checking only the Go struct would
	// let pagination.yaml move a cursor field into required:, which makes the
	// last page unrepresentable for every generated client.
	for _, side := range []struct {
		name   string
		fields map[string]jsonshape.Field
	}{
		{name: "generated", fields: generated},
		{name: "hand-written", fields: handWritten},
	} {
		// Presence is required first: a missing key reads as the zero
		// jsonField, whose OmitEmpty is false, so an absent items would
		// satisfy the assertion below rather than fail it.
		require.Contains(t, side.fields, "items", side.name)
		require.Contains(t, side.fields, "nextPageToken", side.name)
		require.Contains(t, side.fields, "nextPageUrl", side.name)

		assert.False(t, side.fields["items"].OmitEmpty, "%s: items is always rendered", side.name)
		assert.True(t, side.fields["nextPageToken"].OmitEmpty, "%s: absent on the last page", side.name)
		assert.True(t, side.fields["nextPageUrl"].OmitEmpty, "%s: absent on the last page", side.name)
	}

	// Types too, for the cursor fields. Names and optionality alone would let
	// pagination.yaml retype nextPageToken to integer, regenerate it as an int,
	// and still pass — while every generated client started rejecting the
	// strings this adapter actually sends.
	//
	// items is excluded deliberately: it is []T on the hand-written side and
	// []interface{} on the generated one by construction, which is the one
	// difference the envelope's genericity requires.
	for _, name := range []string{"nextPageToken", "nextPageUrl"} {
		assert.Equal(t, handWritten[name].Kind, generated[name].Kind, "%s type", name)
	}
}

func TestPageParameters_ShouldMatchTheHandWrittenNames(t *testing.T) {
	t.Parallel()

	// ARRANGE — the parameter names the spec publishes.
	raw, err := os.ReadFile("pagination.yaml")
	require.NoError(t, err)

	type declaredParameter struct {
		Name   string `yaml:"name"`
		In     string `yaml:"in"`
		Schema struct {
			Type    string `yaml:"type"`
			Minimum int    `yaml:"minimum"`
		} `yaml:"schema"`
	}

	var doc struct {
		Components struct {
			Parameters map[string]declaredParameter `yaml:"parameters"`
		} `yaml:"components"`
	}

	require.NoError(t, yaml.Unmarshal(raw, &doc))

	// ACT
	declared := make(map[string]declaredParameter, len(doc.Components.Parameters))
	for _, parameter := range doc.Components.Parameters {
		declared[parameter.Name] = parameter
	}

	// ASSERT — a spec that documents a parameter the handler does not read is
	// worse than no spec: a client would send it and be silently ignored.
	assert.Equal(t, "query", declared[pagination.ParamPageSize].In)
	assert.Equal(t, "query", declared[pagination.ParamPageToken].In)
	assert.Len(t, declared, 2, "every published parameter should have a constant handlers read")

	// The schemas matter as much as the names. pageSize retyped to string would
	// generate clients that send "50" to a handler that answers 400 to anything
	// strconv.Atoi refuses, and the name-only assertions above would not notice.
	assert.Equal(t, "integer", declared[pagination.ParamPageSize].Schema.Type)
	assert.Equal(t, 1, declared[pagination.ParamPageSize].Schema.Minimum,
		"parseSize rejects anything below 1 rather than clamping up to it")
	assert.Equal(t, "string", declared[pagination.ParamPageToken].Schema.Type)
}

func TestJob_ShouldOmitAbsentOptionalFields(t *testing.T) {
	t.Parallel()

	// ARRANGE — a job that has not finished, so neither completion time nor
	// error exists yet. createdAt is required, so it is set: leaving it zero
	// would emit the very zero timestamp this test exists to catch.
	job := Job{
		Id:        "01J9Z6X2QK7N4V8TA3B5C6D7E8",
		Status:    Pending,
		CreatedAt: time.Date(2026, time.July, 18, 9, 2, 36, 0, time.UTC),
	}

	// ACT
	encoded, err := json.Marshal(job)
	require.NoError(t, err)

	// ASSERT — the whole document, not a substring scan: omitempty does nothing
	// for a struct or a time.Time, so without pointers these would render as a
	// zero timestamp and an empty error object, and "absent" would be
	// indistinguishable from "failed with a blank problem".
	assert.JSONEq(t, `{
		"id":        "01J9Z6X2QK7N4V8TA3B5C6D7E8",
		"status":    "pending",
		"createdAt": "2026-07-18T09:02:36Z"
	}`, string(encoded))
}
