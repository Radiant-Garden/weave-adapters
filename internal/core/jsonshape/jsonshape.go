// Package jsonshape describes a Go struct by the JSON fields a client actually
// observes, so two descriptions of one wire format can be compared.
//
// It exists because this repo defines several wire shapes twice: once
// hand-written in Go (apierror.Problem, pagination.Page, health.Response,
// dhcpwindows.Scope) and once in an OpenAPI document that oapi-codegen
// generates a second Go type from. A client cannot tell which side produced a
// response, so the two must not differ — and nothing but a test enforces that.
//
// Test-only, like internal/core/events/testing, and a normal package for the
// same reason: api/common and api/dhcp-windows both need it, and a _test.go
// file cannot be imported. It lived in api/common/common_test.go first; the
// copy made for the adapter spec silently lost the embedded-struct branch
// below, which is what moved it here.
package jsonshape

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Field is one field's observable contract: what a client sees.
type Field struct {
	Name      string
	OmitEmpty bool
	Kind      string
}

// Of describes a struct value by its JSON fields, keyed by JSON name.
//
// Two cases are handled that a naive scan of json tags misses, and both would
// otherwise let a real wire-format difference compare equal:
//
//   - An exported field with no json tag still serializes, under its Go name.
//   - An embedded struct promotes its fields onto the wire rather than nesting
//     them, so its field set belongs to the parent.
//
// It requires a struct: only a struct has a JSON field set to describe.
func Of(t *testing.T, target any) map[string]Field {
	t.Helper()

	typ := reflect.TypeOf(target)
	require.NotNil(t, typ, "a nil interface has no JSON field set")
	require.Equal(t, reflect.Struct, typ.Kind(), "only struct types have a JSON field set")

	fields := make(map[string]Field, typ.NumField())

	for field := range typ.Fields() {
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}

		parts := strings.Split(tag, ",")
		name := parts[0]

		// Embedded and untagged: encoding/json promotes an anonymous struct's
		// fields onto the parent rather than nesting them.
		//
		// This runs BEFORE the exported check, and that order is the whole
		// point. reflect reports an embedded field by its *type* name, so
		// embedding an unexported struct type gives an unexported field — while
		// encoding/json still promotes that type's exported fields. Checking
		// IsExported first therefore drops them, and both sides of a comparison
		// drop them identically, so the field set compares equal while the wire
		// formats differ. Verified against the encoder in
		// TestOf_ShouldAgreeWithEncodingJSON, which is how the ordering bug was
		// found in the first place.
		//
		// A tagged anonymous field is excluded by the name == "" guard: json
		// treats it as an ordinary field under the tag name, not as a promotion.
		if name == "" && field.Anonymous && field.Type.Kind() == reflect.Struct {
			for embedded, describe := range Of(t, reflect.Zero(field.Type).Interface()) {
				_, clash := fields[embedded]
				require.False(t, clash, "two fields share the JSON name %q", embedded)
				fields[embedded] = describe
			}

			continue
		}

		if !field.IsExported() {
			continue
		}

		if name == "" {
			// Untagged: encoding/json falls back to the Go name.
			name = field.Name
		}

		_, clash := fields[name]
		require.False(t, clash, "two fields share the JSON name %q", name)

		fields[name] = Field{
			Name:      name,
			OmitEmpty: slices.Contains(parts[1:], "omitempty"),
			Kind:      kindOf(field.Type),
		}
	}

	return fields
}

// Names returns the JSON names in a field set, sorted so two sets compare by
// content rather than by map iteration order.
func Names(fields map[string]Field) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// kindOf renders a type as the shape a client observes. Pointers are
// transparent on the wire and named types are not observable at all, so both
// reduce to the underlying kind — otherwise apierror.FieldError and
// common.FieldError would differ over something no client can see.
func kindOf(typ reflect.Type) string {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	if typ.Kind() == reflect.Slice {
		return "[]" + kindOf(typ.Elem())
	}

	return typ.Kind().String()
}
