/*
Testing: common.go

Pending:

Tested:
  errors.yaml -> errors.gen.go
    - TestProblem_ShouldMatchTheHandWrittenStruct: generated Problem and apierror.Problem carry the same JSON fields.
    - TestProblem_ShouldRoundTripThroughTheHandWrittenStruct: a populated problem survives both types byte-identically.
    - TestFieldError_ShouldMatchTheHandWrittenStruct: same for the errors[] element.
    - TestProblemType_ShouldMatchTheLiveTaxonomy: the enum and the Go taxonomy list exactly the same codes.
  pagination.yaml -> pagination.gen.go
    - TestPageEnvelope_ShouldMatchTheHandWrittenStruct: generated envelope and pagination.Page agree on field names and optionality.
    - TestPageParameters_ShouldMatchTheHandWrittenNames: the spec's query parameters are the constants handlers actually read.
  jobs.yaml -> jobs.gen.go
    - TestJob_ShouldOmitAbsentOptionalFields: a pending job carries no zero timestamp and no empty error object.

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
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

// jsonField is one field's observable contract: what a client sees.
type jsonField struct {
	Name      string
	OmitEmpty bool
	Kind      string
}

// jsonFieldsOf describes a struct type by its JSON fields.
func jsonFieldsOf(t *testing.T, target any) map[string]jsonField {
	t.Helper()

	typ := reflect.TypeOf(target)
	require.Equal(t, reflect.Struct, typ.Kind(), "only struct types have a JSON field set")

	fields := make(map[string]jsonField, typ.NumField())

	for field := range typ.Fields() {
		tag := field.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue
		}

		parts := strings.Split(tag, ",")
		name := parts[0]

		fields[name] = jsonField{
			Name:      name,
			OmitEmpty: slices.Contains(parts[1:], "omitempty"),
			Kind:      kindOf(field.Type),
		}
	}

	return fields
}

// kindOf renders a type as the shape a client observes. Pointers are
// transparent on the wire, and named types are not observable at all, so both
// are reduced to the underlying kind.
func kindOf(typ reflect.Type) string {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ.Kind() == reflect.Slice {
		return "[]" + kindOf(typ.Elem())
	}

	return typ.Kind().String()
}

func TestProblem_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	generated := jsonFieldsOf(t, Problem{})
	handWritten := jsonFieldsOf(t, apierror.Problem{})

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
	assert.Equal(t, jsonFieldsOf(t, apierror.FieldError{}), jsonFieldsOf(t, FieldError{}))
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
	generated := jsonFieldsOf(t, PageEnvelope{})
	handWritten := jsonFieldsOf(t, pagination.Page[struct{}]{})

	// ACT
	generatedNames := fieldNames(generated)
	handWrittenNames := fieldNames(handWritten)

	// ASSERT
	assert.ElementsMatch(t, handWrittenNames, generatedNames)

	// items is required on both sides; the two cursor forms are optional and
	// absent together on the last page.
	assert.False(t, generated["items"].OmitEmpty, "items is required by the spec")
	assert.False(t, handWritten["items"].OmitEmpty, "items must always be rendered")
	assert.True(t, handWritten["nextPageToken"].OmitEmpty)
	assert.True(t, handWritten["nextPageUrl"].OmitEmpty)
}

func fieldNames(fields map[string]jsonField) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}

	return names
}

func TestPageParameters_ShouldMatchTheHandWrittenNames(t *testing.T) {
	t.Parallel()

	// ARRANGE — the parameter names the spec publishes.
	raw, err := os.ReadFile("pagination.yaml")
	require.NoError(t, err)

	var doc struct {
		Components struct {
			Parameters map[string]struct {
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}

	require.NoError(t, yaml.Unmarshal(raw, &doc))

	// ACT
	declared := make(map[string]string, len(doc.Components.Parameters))
	for _, parameter := range doc.Components.Parameters {
		declared[parameter.Name] = parameter.In
	}

	// ASSERT — a spec that documents a parameter the handler does not read is
	// worse than no spec: a client would send it and be silently ignored.
	assert.Equal(t, "query", declared[pagination.ParamPageSize])
	assert.Equal(t, "query", declared[pagination.ParamPageToken])
	assert.Len(t, declared, 2, "every published parameter should have a constant handlers read")
}

func TestJob_ShouldOmitAbsentOptionalFields(t *testing.T) {
	t.Parallel()

	// ARRANGE — a job that has not finished, so neither completion time nor
	// error exists yet.
	job := Job{Id: "01J9Z6X2QK7N4V8TA3B5C6D7E8", Status: Pending}

	// ACT
	encoded, err := json.Marshal(job)
	require.NoError(t, err)

	// ASSERT — omitempty does nothing for a struct or a time.Time, so without
	// pointers these would render as a zero timestamp and an empty error
	// object, and "absent" would be indistinguishable from "failed with a blank
	// problem".
	assert.NotContains(t, string(encoded), "completedAt")
	assert.NotContains(t, string(encoded), "error")
	assert.Contains(t, string(encoded), `"status":"pending"`)
}
