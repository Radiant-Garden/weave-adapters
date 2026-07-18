/*
Testing: page.go

Pending:

Tested:
  New
    - TestNew_ShouldPanicOnAnUnusableConfiguration: wiring mistakes fail at start, not per request.
  Parse
    - TestParse_ShouldDefaultAnAbsentPageSize: no pageSize means the collection's default.
    - TestParse_ShouldClampPageSize: a request above the max is served the max, not rejected — including one too large for an int.
    - TestParse_ShouldRejectAnUnusablePageSize: non-numeric and non-positive values are a 400.
    - TestParse_ShouldResumeFromAPageToken: a minted token becomes the After key.
    - TestParse_ShouldRejectAMalformedPageToken: a bad token is a 400 problem+json, never a panic or a full scan.
    - TestParse_ShouldReportEveryInvalidFieldAtOnce: both failures in one response.
    - TestParse_ShouldNotNameTheFailureReasonForABadToken: one message for all four ways a token can be wrong.
    - TestParse_ShouldTakeTheFirstValueOfARepeatedParameter: duplicates resolve first-wins, not to a 400.
  NextToken
    - TestNextToken_ShouldMintATokenThisPaginatorCanParse: the encode/parse pair closes.
    - TestNextToken_ShouldMintNoTokenForAnEmptyKey: the exhausted-listing case composes into a last-page envelope.
    - TestNextToken_ShouldNotBeParseableByAnotherCollection: scopes do not cross.
  NewPage / Page.MarshalJSON
    - TestNewPage_ShouldOmitNextPageTokenOnTheLastPage: absence is the end-of-listing signal.
    - TestNewPage_ShouldRenderAnEmptyCollectionAsAnArray: never "items": null.
    - TestPage_ShouldRenderItemsAsAnArrayHoweverItWasBuilt: the guarantee survives a struct literal, not just NewPage.
    - TestPage_ShouldMarshalThroughAPointer: the value receiver keeps &page rendering identically.

Tested elsewhere:
  The cursor encoding itself is covered in token_test.go. That WriteError emits
  exactly one API-903 event for a multi-field failure is apierror's contract,
  asserted in its TestWriteError_ShouldEmitOneEventForAValidationFailure. The
  full list -> ETag -> 304 -> page 2 composition is Phase 7's demo-resource test.

Declined:
  parseSize / parseToken — unexported, and every branch of both is reached
  through Parse, which is the boundary that actually has to be right.

Additional Remarks:
  Parse's errors are asserted as rendered problem+json rather than by inspecting
  the *apierror.Error, because the contract this package owes a client is the
  400 body and its errors[] entries. Asserting the internal type would pass even
  if the error rendered as a 500.
*/

package pagination

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// leasePages is the paginator under test: a collection defaulting to 100 items
// and refusing to serve more than 500.
var leasePages = New("leases", 100, 500)

func TestNew_ShouldPanicOnAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scope       string
		def, maxVal int
	}{
		{name: "should panic on an empty scope", scope: "", def: 100, maxVal: 500},
		{name: "should panic on a non-positive default", scope: "leases", def: 0, maxVal: 500},
		{name: "should panic on a non-positive max", scope: "leases", def: 100, maxVal: 0},
		{name: "should panic when the default exceeds the max", scope: "leases", def: 600, maxVal: 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT — a wiring mistake must be impossible to deploy.
			assert.Panics(t, func() { New(tt.scope, tt.def, tt.maxVal) })
		})
	}
}

func TestParse_ShouldDefaultAnAbsentPageSize(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	params, err := leasePages.Parse(url.Values{})

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, 100, params.Size)
	assert.Empty(t, params.After, "no token means start at the beginning")
}

func TestParse_ShouldClampPageSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "should honor a size within the limits", raw: "25", want: 25},
		{name: "should honor the minimum size", raw: "1", want: 1},
		{name: "should honor the max exactly", raw: "500", want: 500},
		{name: "should clamp a size above the max", raw: "5000", want: 500},
		{name: "should clamp a size too large for an int", raw: "99999999999999999999", want: 500},
		{name: "should default an empty size", raw: "", want: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			params, err := leasePages.Parse(url.Values{ParamPageSize: {tt.raw}})

			// ASSERT — clamping is silent by design; nextPageToken, not the item
			// count, is what tells the client whether more remain.
			require.NoError(t, err)
			assert.Equal(t, tt.want, params.Size)
		})
	}
}

func TestParse_ShouldRejectAnUnusablePageSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, raw, wantMessage string
	}{
		{name: "should reject a non-numeric size", raw: "abc", wantMessage: "must be an integer"},
		{name: "should reject a float", raw: "10.5", wantMessage: "must be an integer"},
		{name: "should reject zero", raw: "0", wantMessage: "must be at least 1"},
		{name: "should reject a negative size", raw: "-5", wantMessage: "must be at least 1"},
		{name: "should reject a negative size too large for an int", raw: "-99999999999999999999", wantMessage: "must be at least 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := leasePages.Parse(url.Values{ParamPageSize: {tt.raw}})

			// ASSERT — there is no honest value to clamp toward, so this is a
			// client fault rather than a silently different query.
			require.Error(t, err)

			problem := renderProblem(t, err)
			assert.Equal(t, http.StatusBadRequest, problem.Status)
			require.Len(t, problem.Errors, 1)
			assert.Equal(t, ParamPageSize, problem.Errors[0].Field)
			assert.Equal(t, tt.wantMessage, problem.Errors[0].Message)
		})
	}
}

func TestParse_ShouldResumeFromAPageToken(t *testing.T) {
	t.Parallel()

	// ARRANGE — the token this endpoint handed out with the previous page.
	token := leasePages.NextToken("lease-0042")

	// ACT
	params, err := leasePages.Parse(url.Values{ParamPageToken: {token}})

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, "lease-0042", params.After)
	assert.Equal(t, 100, params.Size, "a token does not carry the size; pageSize still applies per request")
}

func TestParse_ShouldRejectAMalformedPageToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, token string
	}{
		{name: "should reject junk", token: "not-a-token"},
		{name: "should reject a tampered token", token: leasePages.NextToken("lease-0042") + "AAAA"},
		{name: "should reject a token from another collection", token: New("scopes", 100, 500).NextToken("scope-7")},
		// The one that would otherwise be a silent full scan rather than a 400.
		{name: "should reject a well-formed cursor carrying no key", token: encodeToken("leases", "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := leasePages.Parse(url.Values{ParamPageToken: {tt.token}})

			// ASSERT — the requirement is explicit: a malformed token is a 400,
			// not a panic and not a silent scan from the top. Asserting the
			// returned After is empty would be vacuous, since Parse returns a
			// zero Params on every error path regardless of what it decoded.
			require.Error(t, err)

			problem := renderProblem(t, err)
			assert.Equal(t, http.StatusBadRequest, problem.Status)
			require.Len(t, problem.Errors, 1)
			assert.Equal(t, ParamPageToken, problem.Errors[0].Field)
		})
	}
}

func TestParse_ShouldReportEveryInvalidFieldAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE — a request that gets both parameters wrong.
	query := url.Values{ParamPageSize: {"abc"}, ParamPageToken: {"not-a-token"}}

	// ACT
	_, err := leasePages.Parse(query)

	// ASSERT — one round trip, every failure, per 03-api-conventions.
	require.Error(t, err)

	problem := renderProblem(t, err)
	require.Len(t, problem.Errors, 2)

	fields := []string{problem.Errors[0].Field, problem.Errors[1].Field}
	assert.ElementsMatch(t, []string{ParamPageSize, ParamPageToken}, fields)
}

func TestParse_ShouldNotNameTheFailureReasonForABadToken(t *testing.T) {
	t.Parallel()

	// ARRANGE — three tokens that fail three different internal checks.
	tokens := []string{
		"not-base64-at-all-%%%",
		leasePages.NextToken("lease-0042")[:6],
		New("scopes", 100, 500).NextToken("scope-7"),
	}

	// ACT
	messages := make([]string, 0, len(tokens))

	for _, token := range tokens {
		_, err := leasePages.Parse(url.Values{ParamPageToken: {token}})
		require.Error(t, err)

		problem := renderProblem(t, err)
		require.Len(t, problem.Errors, 1)
		messages = append(messages, problem.Errors[0].Message)
	}

	// ASSERT — one message for every reason. The client's only recovery is the
	// same in all three cases, and distinguishing them would describe our token
	// encoding to anyone probing it.
	assert.Equal(t, messages[0], messages[1])
	assert.Equal(t, messages[1], messages[2])
}

func TestParse_ShouldTakeTheFirstValueOfARepeatedParameter(t *testing.T) {
	t.Parallel()

	// ARRANGE — a client that appended each parameter twice.
	query := url.Values{
		ParamPageSize:  {"25", "abc"},
		ParamPageToken: {leasePages.NextToken("lease-0042"), "junk"},
	}

	// ACT
	params, err := leasePages.Parse(query)

	// ASSERT — first wins, matching net/http and every OpenAPI default, so the
	// trailing junk neither wins nor turns a harmless duplicate into a 400.
	require.NoError(t, err)
	assert.Equal(t, 25, params.Size)
	assert.Equal(t, "lease-0042", params.After)
}

func TestNextToken_ShouldMintATokenThisPaginatorCanParse(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — the loop a polling client actually runs.
	params, err := leasePages.Parse(url.Values{ParamPageToken: {leasePages.NextToken("lease-0099")}})

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, "lease-0099", params.After)
}

func TestNextToken_ShouldMintNoTokenForAnEmptyKey(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — what a handler passes when the page it just built is
	// empty, or when the listing is exhausted.
	page := NewPage([]string{}, leasePages.NextToken(""))

	// ASSERT — the degenerate case composes into a correct last-page envelope
	// instead of a poison token that the next request would 400 on.
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":[]}`, string(encoded))
}

func TestNextToken_ShouldNotBeParseableByAnotherCollection(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopePages := New("scopes", 100, 500)

	// ACT
	_, err := scopePages.Parse(url.Values{ParamPageToken: {leasePages.NextToken("lease-0042")}})

	// ASSERT — a token is bound to the listing that minted it.
	require.Error(t, err)
}

func TestNewPage_ShouldOmitNextPageTokenOnTheLastPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		next          string
		wantSerialize bool
	}{
		{name: "should omit the token when the listing is exhausted", next: "", wantSerialize: false},
		{name: "should carry the token when more pages remain", next: "abc", wantSerialize: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			encoded, err := json.Marshal(NewPage([]string{"a", "b"}, tt.next))
			require.NoError(t, err)

			// ASSERT — presence of the field, not a full page of items, is what
			// tells a client to ask again.
			assert.Equal(t, tt.wantSerialize, containsKey(t, encoded, "nextPageToken"))
		})
	}
}

func TestNewPage_ShouldRenderAnEmptyCollectionAsAnArray(t *testing.T) {
	t.Parallel()

	// ARRANGE — what a handler returns when a filter matches nothing.
	var none []string

	// ACT
	encoded, err := json.Marshal(NewPage(none, ""))
	require.NoError(t, err)

	// ASSERT — a client iterates items directly; null there is a crash, not an
	// empty listing.
	assert.JSONEq(t, `{"items":[]}`, string(encoded))
}

func TestPage_ShouldRenderItemsAsAnArrayHoweverItWasBuilt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		page Page[string]
	}{
		{name: "should normalize a zero value", page: Page[string]{}},
		{name: "should normalize a struct literal", page: Page[string]{NextPageToken: "abc"}},
		{name: "should normalize an explicit nil", page: Page[string]{Items: nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT — Page and its fields are exported, so nothing forces a
			// handler through NewPage.
			encoded, err := json.Marshal(tt.page)
			require.NoError(t, err)

			// ASSERT — the guarantee has to hold however the value was built,
			// or it is not a guarantee.
			assert.Contains(t, string(encoded), `"items":[]`)
			assert.NotContains(t, string(encoded), "null")
		})
	}
}

func TestPage_ShouldMarshalThroughAPointer(t *testing.T) {
	t.Parallel()

	// ARRANGE — handlers often encode &page rather than the value.
	page := NewPage([]string{"a"}, "next")

	// ACT
	encoded, err := json.Marshal(&page)
	require.NoError(t, err)

	// ASSERT — a value receiver keeps the method in the pointer's method set,
	// so both forms render identically.
	assert.JSONEq(t, `{"items":["a"],"nextPageToken":"next"}`, string(encoded))
}

// renderProblem writes err through apierror and decodes the response body, so
// assertions are made against the bytes a client receives rather than against
// the internal error type.
// It deliberately does not install an event recorder. Doing so mutates the
// global emitter hook, which would cost every test here its t.Parallel, and the
// error-to-event pairing is apierror's contract, asserted in its write_test.go.
func renderProblem(t *testing.T, err error) apierror.Problem {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/leases", nil)

	apierror.WriteError(recorder, request, err)

	var problem apierror.Problem
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&problem))
	require.Equal(t, apierror.ContentType, recorder.Header().Get("Content-Type"))
	require.Equal(t, apierror.TypeFor(events.CodeValidationFailed), problem.Type)

	return problem
}

// containsKey reports whether encoded JSON carries the named top-level key,
// which is how an omitempty field is observed from the outside.
func containsKey(t *testing.T, encoded []byte, key string) bool {
	t.Helper()

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))

	_, ok := fields[key]

	return ok
}
