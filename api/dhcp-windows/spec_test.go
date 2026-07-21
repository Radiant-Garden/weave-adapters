/*
Testing: spec.go

Pending:

	That the paths the spec declares are the paths the server actually mounts.
	Only /api/v1/health and /openapi.yaml are routed today; /api/v1/scopes is
	specified here first, on purpose, and the handler follows in the next phase.
	The assertion becomes possible — and worth making — once it exists, because
	that is the moment "spec-first" could quietly become "spec-fiction".

Tested:

	Spec
	  - TestSpec_ShouldEmbedTheDocumentOnDisk: what the binary serves is the file in this directory.
	  - TestSpec_ShouldBeAServableDocumentWithPaths: unlike api/common, this one carries paths.
	openapi.yaml -> openapi.gen.go
	  - TestScope_ShouldMatchTheHandWrittenStruct: generated Scope and dhcpwindows.Scope carry the same JSON fields.
	  - TestScopeCreate_ShouldNotAcceptDerivedFields: a client cannot assert scopeId, wadaptId, addressFamily or superscopeName.
	  - TestScopeCreate_ShouldBeASubsetOfTheServedScope: every field a client may send comes back under the same name and shape.
	  - TestScopeCreate_ShouldMatchTheHandWrittenInput: the spec and adapter.ScopeInput agree field for field — the handler decodes into it and rejects unknown fields, so a drift here 400s a spec-conformant client.
	  - TestScopeUpdate_ShouldMatchTheHandWrittenStruct: the generated ScopeUpdate and adapter.ScopeUpdate agree, pointer-vs-value aside — the merge-update contract.
	  - TestScopeUpdate_ShouldNotAcceptDerivedFields: an update cannot assert scopeId, subnetMask, wadaptId or any other immutable/derived field, and every field it carries is optional.
	  - TestScopeUpdate_ShouldBeASubsetOfTheServedScope: every field a client may PATCH comes back under the same name and shape.
	  - TestScope_ShouldRoundTripThroughTheHandWrittenStruct: a populated scope survives both types byte-identically.
	  - TestScopeList_ShouldMatchTheSharedPageEnvelope: the adapter's list schema is the shared envelope with a concrete item.
	  - TestHealthResponse_ShouldMatchTheHandWrittenStruct: same for the health shape, including its component entries.
	  - TestHealthStatus_ShouldMatchTheLiveVocabulary: the enum and the Go status constants agree.
	  - TestAddressFamily_ShouldMatchTheAdapterConstant: the only declared family is the one the client sets.
	openapi.yaml (read as YAML)
	  - TestWadaptIDPattern_ShouldMatchTheDerivedForm: the published regexp accepts a real derived ID and pins its length.
	  - TestQueryParameters_ShouldMatchTheHandWrittenNames: the operation publishes exactly its own two parameters, and reuses the shared pagination ones by ref rather than restating them.

Tested elsewhere:

	The behaviour behind these shapes belongs to the packages that own it —
	internal/adapters/dhcpwindows for the scope model and the derivation,
	internal/core/health for the status vocabulary, internal/core/pagination for
	the envelope. Nothing here exercises logic; these assert that two
	descriptions of one wire format agree.

	That the served route returns these bytes with the right media type:
	internal/core/httpserver, which owns the route. It passes stand-in bytes
	rather than importing this package, because core must never import an
	adapter.

Declined:

	Re-validating the document against the OpenAPI meta-schema. oapi-codegen
	parses it on every `task generate`, and `generate-check` runs that in CI, so
	an invalid spec or a broken $ref already fails the gate — earlier and with a
	better message than a second validator here would give.

	A test file for openapi.gen.go. It is generated; what is worth asserting
	about it is that it matches the hand-written types, which is this file.

Additional Remarks:

	Field types are compared by JSON name, optionality and kind rather than by
	reflect.Type, for the reason api/common gives: the two sides legitimately use
	different named types for one wire shape (ScopeState vs a plain string), and
	requiring identical Go types would fail on a difference no client can
	observe.
*/
package dhcpwindows

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	adapter "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows"
	"github.com/radiantgarden/weave-adapters/internal/core/health"
	"github.com/radiantgarden/weave-adapters/internal/core/jsonshape"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

// specDocument is the part of openapi.yaml these tests read directly. The
// generated Go types cannot answer questions about the spec's own vocabulary —
// a published regexp or a parameter name is not a Go type — so those are read
// from the document itself.
type specDocument struct {
	Paths map[string]struct {
		Get struct {
			Parameters []struct {
				Ref  string `yaml:"$ref"`
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"get"`
	} `yaml:"paths"`
	Components struct {
		Parameters map[string]struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
		} `yaml:"parameters"`
		Schemas map[string]struct {
			Enum       []string `yaml:"enum"`
			Properties map[string]struct {
				Pattern string `yaml:"pattern"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

// readSpec parses the embedded document. It reads Spec rather than the file so
// a test cannot pass against a document the binary does not actually serve.
func readSpec(t *testing.T) specDocument {
	t.Helper()

	var doc specDocument
	require.NoError(t, yaml.Unmarshal(Spec(), &doc))

	return doc
}

func TestSpec_ShouldEmbedTheDocumentOnDisk(t *testing.T) {
	t.Parallel()

	// ARRANGE
	onDisk, err := os.ReadFile("openapi.yaml")
	require.NoError(t, err)

	// ACT / ASSERT — the embed is what makes the release a single .exe, and it
	// is only trustworthy if the served bytes are the reviewed bytes.
	require.NotEmpty(t, Spec())
	assert.Equal(t, onDisk, Spec())
}

func TestSpec_ShouldBeAServableDocumentWithPaths(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	doc := readSpec(t)

	// ASSERT — this is what separates the adapter spec from api/common, which is
	// a component library carrying no paths and is deliberately not servable.
	// A document that lost its paths would still parse and still generate
	// models, while describing an API with no operations in it.
	assert.Contains(t, doc.Paths, "/api/v1/health")
	assert.Contains(t, doc.Paths, "/api/v1/scopes")
	assert.Contains(t, doc.Paths, "/api/v1/scopes/{wadaptId}",
		"a create's Location header points here, so it has to exist")
	assert.Contains(t, doc.Paths, "/openapi.yaml",
		"the spec route is served, so it is documented like any other")
}

func TestScopeCreate_ShouldNotAcceptDerivedFields(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	create := jsonshape.Of(t, ScopeCreate{})

	// ASSERT — the request body must not carry anything the server derives.
	// Accepting scopeId would let a client assert an identity the server is
	// about to compute from the range and mask, and disagree with it; accepting
	// wadaptId would let a client choose an identity derived from a provisioned
	// server name it cannot see. Both are the kind of field that gets added
	// "for convenience" and then has to be honoured forever.
	for _, derived := range []string{"scopeId", "wadaptId", "addressFamily", "superscopeName"} {
		assert.NotContains(t, create, derived,
			"%s is derived or unsupported; a client must not be able to assert it", derived)
	}

	// The fields Windows actually requires on Add-DhcpServerv4Scope, and nothing
	// optional masquerading as required.
	for _, required := range []string{"name", "startRange", "endRange", "subnetMask"} {
		require.Contains(t, create, required)
		assert.False(t, create[required].OmitEmpty, "%s is required by the cmdlet", required)
	}

	// Everything else is optional, so an omitted value stays the DHCP server's
	// default rather than one the adapter substituted.
	for _, optional := range []string{"description", "leaseDurationSeconds", "state", "type"} {
		require.Contains(t, create, optional)
		assert.True(t, create[optional].OmitEmpty, "%s must be omissible", optional)
	}
}

func TestScopeCreate_ShouldBeASubsetOfTheServedScope(t *testing.T) {
	t.Parallel()

	// ARRANGE — every field a client may send must exist on the representation
	// it gets back, under the same name and shape. A create input that named a
	// field differently from the read model would make round-tripping a scope
	// through this API silently lossy.
	create := jsonshape.Of(t, ScopeCreate{})
	served := jsonshape.Of(t, Scope{})

	// ACT / ASSERT
	for name, field := range create {
		require.Contains(t, served, name, "ScopeCreate.%s has no counterpart on Scope", name)
		assert.Equal(t, served[name].Kind, field.Kind, "%s changes type between create and read", name)
	}
}

func TestScopeCreate_ShouldMatchTheHandWrittenInput(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	generated := jsonshape.Of(t, ScopeCreate{})
	handWritten := jsonshape.Of(t, adapter.ScopeInput{})

	// ASSERT — the create handler decodes straight into adapter.ScopeInput, so
	// this struct *is* the request contract. A field the spec promises and the
	// input does not carry is worse than a mismatch on the read side: the
	// handler rejects unknown fields, so a client following the published spec
	// would get a 400 for a field the document told it to send.
	assert.Equal(t, handWritten, generated)
}

func TestScopeUpdate_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	generated := jsonshape.Of(t, ScopeUpdate{})
	handWritten := jsonshape.Of(t, adapter.ScopeUpdate{})

	// ASSERT — the merge-update body the handler decodes into. The hand-written
	// side uses pointers so absent is distinct from provided, and the generated
	// side uses value types with omitempty; jsonshape sees through both to the
	// same wire kind, so a real field-set or optionality drift still fails here.
	assert.Equal(t, handWritten, generated)
}

func TestScopeUpdate_ShouldNotAcceptDerivedFields(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	update := jsonshape.Of(t, ScopeUpdate{})

	// ASSERT — the identity inputs must not be settable on an update: changing
	// them would move the scope's derived identity, so they are absent from the
	// schema and DisallowUnknownFields rejects a client that sends one.
	for _, derived := range []string{"scopeId", "subnetMask", "wadaptId", "addressFamily", "superscopeName"} {
		assert.NotContains(t, update, derived,
			"%s is immutable or derived; a client must not be able to set it on an update", derived)
	}

	// A merge changes only what is sent, so every field it carries is optional.
	for name, field := range update {
		assert.True(t, field.OmitEmpty,
			"ScopeUpdate.%s must be omissible, or an absent field would not be left unchanged", name)
	}
}

func TestScopeUpdate_ShouldBeASubsetOfTheServedScope(t *testing.T) {
	t.Parallel()

	// ARRANGE — every field a client may PATCH must exist on the representation it
	// gets back, under the same name and shape, or updating a scope through this
	// API would be silently lossy.
	update := jsonshape.Of(t, ScopeUpdate{})
	served := jsonshape.Of(t, Scope{})

	// ACT / ASSERT
	for name, field := range update {
		require.Contains(t, served, name, "ScopeUpdate.%s has no counterpart on Scope", name)
		assert.Equal(t, served[name].Kind, field.Kind, "%s changes type between update and read", name)
	}
}

func TestScope_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	generated := jsonshape.Of(t, Scope{})
	handWritten := jsonshape.Of(t, adapter.Scope{})

	// ASSERT — two descriptions of one wire format. A client cannot tell which
	// produced a response, so a field present in one and not the other, or
	// omitempty on one side only, is a contract the adapter does not honour.
	assert.Equal(t, handWritten, generated)
}

func TestScope_ShouldRoundTripThroughTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE — every field populated, including the two optional ones, so an
	// omission on either side shows up as a difference rather than as a shared
	// zero. The values are the ones captured from the WS2022 host.
	original := adapter.Scope{
		WadaptID:             "8k2f5r9tc0hqm",
		ScopeID:              "192.168.178.0",
		SubnetMask:           "255.255.255.0",
		StartRange:           "192.168.178.10",
		EndRange:             "192.168.178.210",
		Name:                 "manual_test_01",
		Description:          "created by hand",
		State:                "Active",
		Type:                 "Dhcp",
		SuperscopeName:       "site-a",
		LeaseDurationSeconds: 691200,
		AddressFamily:        adapter.AddressFamilyIPv4,
	}

	fromHandWritten, err := json.Marshal(original)
	require.NoError(t, err)

	// ACT — decode into the generated type and re-encode.
	var viaGenerated Scope
	require.NoError(t, json.Unmarshal(fromHandWritten, &viaGenerated))

	fromGenerated, err := json.Marshal(viaGenerated)
	require.NoError(t, err)

	// ASSERT — a field the generated type lacks would be dropped here, and a
	// renamed tag would move it.
	assert.JSONEq(t, string(fromHandWritten), string(fromGenerated))
}

func TestScopeList_ShouldMatchTheSharedPageEnvelope(t *testing.T) {
	t.Parallel()

	// ARRANGE — the adapter's list schema against the envelope core mints.
	generated := jsonshape.Of(t, ScopeList{})
	handWritten := jsonshape.Of(t, pagination.Page[adapter.Scope]{})

	// ACT / ASSERT — same field names. An adapter that invented its own envelope
	// could not be paged by weave's generic link walker at all.
	assert.Equal(t, jsonshape.Names(handWritten), jsonshape.Names(generated))

	// Optionality on both sides. Moving a cursor field into required: makes the
	// last page unrepresentable for every generated client; dropping omitempty
	// on items would render "items": null, which clients iterate directly.
	for _, side := range []struct {
		name   string
		fields map[string]jsonshape.Field
	}{
		{name: "generated", fields: generated},
		{name: "hand-written", fields: handWritten},
	} {
		require.Contains(t, side.fields, "items", side.name)
		require.Contains(t, side.fields, "nextPageToken", side.name)
		require.Contains(t, side.fields, "nextPageUrl", side.name)

		assert.False(t, side.fields["items"].OmitEmpty, "%s: items is always rendered", side.name)
		assert.True(t, side.fields["nextPageToken"].OmitEmpty, "%s: absent on the last page", side.name)
		assert.True(t, side.fields["nextPageUrl"].OmitEmpty, "%s: absent on the last page", side.name)
	}

	// Unlike the shared envelope, this one has a concrete item type, so the
	// items kind is comparable rather than []interface{} by construction.
	assert.Equal(t, handWritten["items"].Kind, generated["items"].Kind)
}

func TestHealthResponse_ShouldMatchTheHandWrittenStruct(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT — weave's health client is generic and tolerates
	// shape differences, which is exactly why a drift here would go unnoticed
	// until an operator read the spec and found it describing a response the
	// adapter does not send.
	assert.Equal(t, jsonshape.Of(t, health.Response{}), jsonshape.Of(t, HealthResponse{}))
	assert.Equal(t, jsonshape.Of(t, health.Component{}), jsonshape.Of(t, HealthComponent{}))
}

func TestHealthStatus_ShouldMatchTheLiveVocabulary(t *testing.T) {
	t.Parallel()

	// ARRANGE — the statuses the running adapter can actually report.
	live := []string{
		string(health.StatusHealthy),
		string(health.StatusUnhealthy),
		string(health.StatusUnavailable),
	}

	// ACT
	declared := readSpec(t).Components.Schemas["HealthStatus"].Enum

	// ASSERT — a status the vocabulary gained but the spec never listed leaves a
	// client unable to recognise a state we report, and one the spec lists that
	// nothing sets is a ghost entry.
	assert.ElementsMatch(t, live, declared)
}

func TestAddressFamily_ShouldMatchTheAdapterConstant(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	declared := readSpec(t).Components.Schemas["AddressFamily"].Enum

	// ASSERT — one entry, and it is the value the client sets on every scope.
	// IPv6 lands additively by adding to both sides at once; listing it here
	// before the client can produce it would promise scopes we never serve.
	assert.Equal(t, []string{adapter.AddressFamilyIPv4}, declared)
}

func TestWadaptIDPattern_ShouldMatchTheDerivedForm(t *testing.T) {
	t.Parallel()

	// ARRANGE — the pattern is contract-visible and unchangeable: weave's
	// keyShape regexp encodes the same alphabet and length, and an exposed ID
	// never changes.
	pattern := readSpec(t).Components.Schemas["Scope"].Properties["wadaptId"].Pattern
	require.NotEmpty(t, pattern, "wadaptId publishes its shape")

	shape, err := regexp.Compile(pattern)
	require.NoError(t, err)

	// ACT / ASSERT — the alphabet is base32hex (RFC 4648 §7), so every one of
	// its 32 characters must be accepted. Testing one sample ID would pass
	// against the standard alphabet's regexp too, which is the mistake this
	// pins: the two alphabets agree on 0-9 and differ everywhere after.
	for _, char := range "0123456789abcdefghijklmnopqrstuv" {
		id := strings.Repeat(string(char), adapter.WadaptIDLength)
		assert.Regexp(t, shape, id, "base32hex character %q should be accepted", char)
	}

	// The length is fixed, not a minimum: 8 bytes of HMAC output always encode
	// to exactly 13 characters, so a shorter or longer value is not an ID this
	// adapter minted.
	assert.NotRegexp(t, shape, strings.Repeat("0", adapter.WadaptIDLength-1))
	assert.NotRegexp(t, shape, strings.Repeat("0", adapter.WadaptIDLength+1))

	// Characters outside the alphabet. 'w' through 'z' are the tail the standard
	// alphabet uses and base32hex does not, so a pattern widened to [0-9a-z]
	// would pass every assertion above and fail here.
	for _, char := range "wxyz" {
		assert.NotRegexp(t, shape, strings.Repeat(string(char), adapter.WadaptIDLength),
			"%q is not in the base32hex alphabet", char)
	}

	// Uppercase: the encoding is lowercase and IDs are never reformatted, so an
	// uppercased ID is not one this adapter minted.
	assert.NotRegexp(t, shape, strings.Repeat("A", adapter.WadaptIDLength))
}

func TestQueryParameters_ShouldMatchTheHandWrittenNames(t *testing.T) {
	t.Parallel()

	// ARRANGE
	doc := readSpec(t)

	// ACT — parameters resolve two ways. The adapter's own are $refs into this
	// document, so their name and location can be read here; the shared
	// pagination ones are $refs into ../common/, which yaml.Unmarshal does not
	// follow, so those are asserted as refs.
	local := make(map[string]string)

	var external []string

	for _, parameter := range doc.Paths["/api/v1/scopes"].Get.Parameters {
		ref := parameter.Ref

		component, isLocal := strings.CutPrefix(ref, "#/components/parameters/")
		if !isLocal {
			external = append(external, ref)

			continue
		}

		declared, ok := doc.Components.Parameters[component]
		require.True(t, ok, "%s refs a parameter this document does not define", ref)

		local[declared.Name] = declared.In
	}

	// The external refs are followed rather than matched as strings, which is
	// what makes this assert the published parameter names instead of two
	// unconnected facts. Comparing the ref path alone would still pass if
	// api/common renamed the name: inside its PageSize component — the adapter
	// would then publish a parameter its handler never reads, which is the exact
	// failure this test is named for.
	for _, ref := range external {
		name, in := resolveExternalParameter(t, ref)
		local[name] = in
	}

	// ASSERT — the whole published set, in one comparison. An undocumented
	// parameter is a contract only its author knows about; an extra documented
	// one is a promise the handler must keep. The pagination names come from the
	// core constants the handler actually parses, so a rename on either side
	// fails here.
	assert.Equal(t, map[string]string{
		pagination.ParamPageSize:  "query",
		pagination.ParamPageToken: "query",
		"scopeId":                 "query",
		"If-None-Match":           "header",
	}, local)

	// The shared parameters are reused from api/common rather than restated. An
	// adapter that copied them could rename or retype one and still generate,
	// while every other adapter kept the original — and the uniform API is what
	// this repo exists to provide.
	assert.ElementsMatch(t, []string{
		"../common/pagination.yaml#/components/parameters/PageSize",
		"../common/pagination.yaml#/components/parameters/PageToken",
	}, external)
}

// resolveExternalParameter follows a $ref into another document in api/ and
// returns the referenced parameter's published name and location.
//
// yaml.Unmarshal does not follow refs, so this does the one hop by hand: the
// ref is "<relative path>#/components/parameters/<component>", and paths in the
// spec are relative to the directory holding it, which is this one.
func resolveExternalParameter(t *testing.T, ref string) (name, in string) {
	t.Helper()

	path, pointer, ok := strings.Cut(ref, "#")
	require.True(t, ok, "%q is not a document-and-pointer ref", ref)

	component, ok := strings.CutPrefix(pointer, "/components/parameters/")
	require.True(t, ok, "%q does not point at a parameter component", ref)

	// The path is a relative $ref out of a spec checked into this repo and read
	// by a test, not caller input; there is no untrusted source for it to come
	// from. Guarded anyway so a ref that tried to leave the api/ tree fails
	// loudly here rather than reading whatever it pointed at.
	require.True(t, strings.HasPrefix(path, "../") && !strings.Contains(path, ".."+string(filepath.Separator)+".."),
		"%q refs outside the api tree", ref)

	raw, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "reading the document %q refs", ref)

	var doc specDocument
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	declared, ok := doc.Components.Parameters[component]
	require.True(t, ok, "%s refs a parameter %q does not define", ref, path)

	return declared.Name, declared.In
}
