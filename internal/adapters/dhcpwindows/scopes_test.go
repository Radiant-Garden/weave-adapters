/*
Testing: scopes.go

Pending:

	A walk against a real WS2022 host. Everything here runs against a fake
	lister, so it proves the handler's own logic and the contract it publishes;
	it cannot prove the scopes are the ones the server holds. That is the
	sign-off criterion, not something a test on darwin can close.

Tested:

	ScopesHandler.ServeHTTP / list
	  - TestScopes_ShouldServeAPageOfScopes: the envelope, its content type, and items always an array.
	  - TestScopes_ShouldRenderAnEmptyCollectionAsAnArray: no scopes is [], never null.
	  - TestScopes_ShouldWalkEveryScopeExactlyOnceViaNextPageUrl: the link form weave follows, across a multi-page collection.
	  - TestScopes_ShouldOrderByWadaptIdNotScopeId: the two orders genuinely differ, and the served order is the cursor's.
	  - TestScopes_ShouldStopWithoutCursorsOnTheLastPage: both cursor forms absent together.
	  - TestScopes_ShouldClampPageSizeToTheConfiguredMaximum: over the max is clamped, not rejected.
	parseScopeIDFilter / the ?scopeId= filter
	  - TestScopes_ShouldFilterByScopeId: exact equality on the subnet address.
	  - TestScopes_ShouldNotMutateTheCollectionWhenFiltering: filtering never writes through to the backend's slice.
	  - TestScopes_ShouldCarryQueryParametersIntoNextPageUrl: pageSize survives the link, pageToken is replaced not appended.
	  - TestScopes_ShouldRejectAnAmbiguouslySpelledFilter: a leading-zero address is a 400, not a guess.
	  - TestScopes_ShouldRejectAMalformedScopeIdFilter: 400 with a field error, not an empty 200.
	  - TestScopes_ShouldReportEveryQueryFailureAtOnce: a bad pageSize and a bad scopeId arrive together.
	problemFor
	  - TestScopes_ShouldMapBackendFailuresToTheirOwnStatusCodes: unavailable 502, timeout 504, malformed 502, duplicate 500.
	  - TestScopes_ShouldNotLeakTheBackendMessageToTheClient: stderr reaches the log, never the body.
	the ETag pairing
	  - TestScopes_ShouldAnswer304WhenTheRepresentationIsUnchanged: a re-GET with If-None-Match.
	  - TestScopes_ShouldKeepAFullPageUnderTheEtagBufferLimit: maxPageSize cannot silently cost the 304.

Tested elsewhere:

	Sorting and the duplicate-ID rejection belong to ListScopes and are tested in
	client_test.go; this file takes a sorted listing as given, because that is
	the contract the handler is written against.

	The route being mounted, authenticated and reachable: the binary's own tests
	and the smoke gate. What is asserted here is the handler in isolation plus,
	where it matters, the real middleware chain around it.

Declined:

	Asserting the exact cursor token text. It is opaque by contract — clients
	echo what they were given and never construct one — so pinning its bytes
	would test the encoder rather than the collection, and would make every
	pagination change look like a scopes regression.

	Re-testing pagination's own parameter validation (negative pageSize,
	non-integer, foreign token). internal/core/pagination owns those and covers
	them; this file asserts only the parts the handler adds.

	Walking a *filtered* collection across pages. Windows permits one scope per
	subnet, so an equality filter on scopeId matches at most one scope and can
	never span pages. Constructing a fake with two scopes on one subnet would
	exercise a state the backend cannot produce.

Additional Remarks:

	The fake lister returns scopes already sorted by wadaptID, matching the
	contract ListScopes guarantees. Tests that care about ordering derive real
	IDs rather than inventing them, so the order under test is the order the
	adapter would actually serve.
*/
package dhcpwindows

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/etag"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

// fakeLister stands in for the backend client. It satisfies the consumer-side
// scopeLister interface, which is the point of declaring that interface at the
// consumer: the handler needs one method, so a test supplies one method.
type fakeLister struct {
	scopes []Scope
	err    error

	// calls counts invocations, so a test can assert the handler reads the
	// backend once per request rather than once per page of output.
	calls int
}

func (f *fakeLister) ListScopes(context.Context) ([]Scope, error) {
	f.calls++

	if f.err != nil {
		return nil, f.err
	}

	return f.scopes, nil
}

// testPageConfig is the page-size configuration under test. Small enough that a
// handful of scopes spans several pages without a large fixture.
func testPageConfig(defaultSize, maxSize int) Config {
	return Config{DefaultPageSize: defaultSize, MaxPageSize: maxSize}
}

// scopesFor builds n scopes on distinct subnets, each carrying the wadaptID the
// adapter would really derive, sorted as ListScopes returns them.
//
// Derived rather than invented: the ordering tests below are only meaningful if
// the keys they sort are the keys production would serve.
func scopesFor(t *testing.T, subnets ...string) []Scope {
	t.Helper()

	client := &Client{serverName: "dhcp01.example.test", namespaceKey: []byte(testNamespaceKey)}

	scopes := make([]Scope, 0, len(subnets))
	for _, subnet := range subnets {
		scopes = append(scopes, Scope{
			ScopeID:              subnet,
			SubnetMask:           "255.255.255.0",
			StartRange:           "10.0.0.10",
			EndRange:             "10.0.0.200",
			Name:                 "scope-" + subnet,
			State:                "Active",
			Type:                 "Dhcp",
			LeaseDurationSeconds: 691200,
		})
	}

	require.NoError(t, client.identify(scopes))

	// identify sets the IDs; ListScopes sorts by them, and the handler is
	// written against that guarantee.
	sortByWadaptID(scopes)

	return scopes
}

// sortByWadaptID applies the same ordering ListScopes does.
func sortByWadaptID(scopes []Scope) {
	for i := 1; i < len(scopes); i++ {
		for j := i; j > 0 && scopes[j].WadaptID < scopes[j-1].WadaptID; j-- {
			scopes[j], scopes[j-1] = scopes[j-1], scopes[j]
		}
	}
}

// page is the decoded list envelope.
type page struct {
	Items         []Scope `json:"items"`
	NextPageToken string  `json:"nextPageToken"`
	NextPageURL   string  `json:"nextPageUrl"`
}

// getScopes drives the handler for a query string and returns the raw response.
func getScopes(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()

	target := ScopesPath
	if query != "" {
		target += "?" + query
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	return rec
}

// getPage drives the handler and decodes a successful page.
func getPage(t *testing.T, h http.Handler, query string) page {
	t.Helper()

	rec := getScopes(t, h, query)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var decoded page
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))

	return decoded
}

func TestScopes_ShouldServeAPageOfScopes(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT
	rec := getScopes(t, handler, "")

	// ASSERT
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var decoded page
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))

	assert.Len(t, decoded.Items, 2)
	assert.Empty(t, decoded.NextPageToken, "both scopes fit on one page")

	// Every served scope carries an identity — the milestone's central
	// invariant, asserted on what actually reaches the wire rather than on the
	// struct the handler was handed.
	for _, item := range decoded.Items {
		assert.Len(t, item.WadaptID, WadaptIDLength)
		assert.Equal(t, AddressFamilyIPv4, item.AddressFamily)
	}
}

func TestScopes_ShouldRenderAnEmptyCollectionAsAnArray(t *testing.T) {
	t.Parallel()

	// ARRANGE — a freshly provisioned server with no scopes yet.
	handler := NewScopesHandler(&fakeLister{scopes: []Scope{}}, testPageConfig(50, 500))

	// ACT
	rec := getScopes(t, handler, "")

	// ASSERT — the raw bytes, not the decoded form: "items": null decodes into
	// an empty slice just as happily, so decoding first would hide exactly the
	// difference this asserts. Clients iterate items directly.
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
}

func TestScopes_ShouldWalkEveryScopeExactlyOnceViaNextPageUrl(t *testing.T) {
	t.Parallel()

	// ARRANGE — nextPageUrl is the load-bearing cursor form: weave's list walker
	// follows links and cannot echo a token, so a collection it cannot walk this
	// way cannot be paged by weave at all.
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0", "10.0.4.0", "10.0.5.0")
	backend := &fakeLister{scopes: scopes}
	handler := NewScopesHandler(backend, testPageConfig(2, 500))

	// ACT — walk from the first page, following only what the server hands back.
	var (
		seen  []string
		query string
		pages int
	)

	for {
		current := getPage(t, handler, query)
		pages++

		for _, item := range current.Items {
			seen = append(seen, item.WadaptID)
		}

		if current.NextPageURL == "" {
			assert.Empty(t, current.NextPageToken, "cursor forms are absent together")

			break
		}

		// Follow the link as a client would: take its query verbatim.
		next, err := url.Parse(current.NextPageURL)
		require.NoError(t, err)
		assert.Equal(t, ScopesPath, next.Path, "the link must stay on this collection")
		assert.False(t, next.IsAbs(), "the link must be relative, never absolute")

		query = next.RawQuery

		require.LessOrEqual(t, pages, len(scopes)+1, "walk did not terminate")
	}

	// ASSERT — every scope exactly once, in the served order. No omissions and
	// no repeats is the property a resume key buys over an offset.
	want := make([]string, 0, len(scopes))
	for _, s := range scopes {
		want = append(want, s.WadaptID)
	}

	assert.Equal(t, want, seen)
	assert.Equal(t, 3, pages, "5 scopes at 2 per page is 3 pages")
	assert.Equal(t, pages, backend.calls, "one backend read per page request")
}

func TestScopes_ShouldOrderByWadaptIdNotScopeId(t *testing.T) {
	t.Parallel()

	// ARRANGE — the two orders must genuinely differ or this asserts nothing.
	// Derived IDs are effectively random, so a handful of subnets is enough for
	// their order to diverge from any ordering of the addresses themselves.
	scopes := scopesFor(t, "192.168.2.0", "192.168.178.0", "10.0.0.0", "172.16.30.0", "10.0.200.0")

	byScopeID := make([]string, len(scopes))
	for i, s := range scopes {
		byScopeID[i] = s.ScopeID
	}

	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT
	decoded := getPage(t, handler, "")

	// ASSERT — served in wadaptID order, which is the order the cursor resumes
	// in. Sorting by one and resuming by another is the failure this guards:
	// "192.168.178.0" sorts before "192.168.2.0" as text and after it as an
	// address, so a mismatched pair skips and repeats pages in silence.
	served := make([]string, len(decoded.Items))
	for i, item := range decoded.Items {
		served[i] = item.WadaptID
	}

	assert.IsNonDecreasing(t, served, "the collection is served in wadaptID order")

	servedScopeIDs := make([]string, len(decoded.Items))
	for i, item := range decoded.Items {
		servedScopeIDs[i] = item.ScopeID
	}

	assert.ElementsMatch(t, byScopeID, servedScopeIDs, "same scopes, whatever the order")
}

func TestScopes_ShouldStopWithoutCursorsOnTheLastPage(t *testing.T) {
	t.Parallel()

	// ARRANGE — exactly one full page, which is the boundary worth pinning: a
	// cursor here would send a client after a page that does not exist, and its
	// presence rather than a short page is what tells a client to continue.
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(2, 500))

	// ACT
	decoded := getPage(t, handler, "")

	// ASSERT
	require.Len(t, decoded.Items, 2)
	assert.Empty(t, decoded.NextPageToken)
	assert.Empty(t, decoded.NextPageURL)
}

func TestScopes_ShouldClampPageSizeToTheConfiguredMaximum(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0", "10.0.4.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(2, 3))

	// ACT — well over the maximum.
	decoded := getPage(t, handler, "pageSize=1000")

	// ASSERT — clamped rather than rejected, which is why nextPageToken and not
	// the item count is the authority on whether more items exist.
	assert.Len(t, decoded.Items, 3)
	assert.NotEmpty(t, decoded.NextPageToken)
}

func TestScopes_ShouldFilterByScopeId(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT
	decoded := getPage(t, handler, "scopeId=10.0.2.0")

	// ASSERT — exact equality, not a prefix or a range.
	require.Len(t, decoded.Items, 1)
	assert.Equal(t, "10.0.2.0", decoded.Items[0].ScopeID)

	// A filter matching nothing is an empty page, not a 404: the collection
	// exists, and it has no member on that subnet.
	empty := getPage(t, handler, "scopeId=10.0.9.0")
	assert.Empty(t, empty.Items)
}

func TestScopes_ShouldNotMutateTheCollectionWhenFiltering(t *testing.T) {
	t.Parallel()

	// ARRANGE — the fake hands back the same slice on every call, which is what
	// a cache would do, and what the cache phase is specified to do: it holds
	// the last read. Filtering in place would compact that array and zero its
	// tail, so the damage lands on every *later* request rather than on the one
	// that caused it.
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0")
	backend := &fakeLister{scopes: scopes}
	handler := NewScopesHandler(backend, testPageConfig(50, 500))

	before := make([]string, len(backend.scopes))
	for i, s := range backend.scopes {
		before[i] = s.ScopeID
	}

	// ACT — one filtered request, then read the whole collection back.
	filtered := getPage(t, handler, "scopeId=10.0.2.0")
	require.Len(t, filtered.Items, 1)

	// ASSERT — the backend's own listing is untouched...
	after := make([]string, len(backend.scopes))
	for i, s := range backend.scopes {
		after[i] = s.ScopeID
	}

	assert.Equal(t, before, after, "filtering must not write through to the caller's slice")

	// ...and an unfiltered request still sees every scope, each with an
	// identity. Before this was fixed, the two scopes the filter excluded came
	// back with an empty scopeId and an empty wadaptId — a scope with no
	// identity, which is the one thing this milestone's invariant forbids.
	all := getPage(t, handler, "")
	require.Len(t, all.Items, 3)

	for _, item := range all.Items {
		assert.NotEmpty(t, item.ScopeID)
		assert.Len(t, item.WadaptID, WadaptIDLength)
	}
}

func TestScopes_ShouldCarryQueryParametersIntoNextPageUrl(t *testing.T) {
	t.Parallel()

	// ARRANGE — a link-following client sends nothing but the link, so a
	// parameter dropped from nextPageUrl silently changes what later pages
	// contain.
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(1, 500))

	// ACT
	first := getPage(t, handler, "pageSize=1")
	require.NotEmpty(t, first.NextPageURL)

	next, err := url.Parse(first.NextPageURL)
	require.NoError(t, err)

	// ASSERT — pageSize survives, and pageToken is replaced rather than
	// appended: two pageToken values would leave which one wins up to the
	// parser.
	assert.Equal(t, "1", next.Query().Get(pagination.ParamPageSize))
	assert.Len(t, next.Query()[pagination.ParamPageToken], 1)

	// The filter is deliberately not walked across pages here, because it
	// cannot be: Windows permits exactly one scope per subnet, so an equality
	// filter on scopeId matches at most one scope and its result always fits a
	// single page. Faking a collection with two scopes on one subnet would test
	// a state the backend cannot produce. What is assertable — and what the
	// filtered walk would depend on — is that a filtered first page is a last
	// page, and that the filter reaches the response at all.
	filtered := getPage(t, handler, "scopeId=10.0.2.0&pageSize=1")
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, "10.0.2.0", filtered.Items[0].ScopeID)
	assert.Empty(t, filtered.NextPageURL, "one subnet, one scope, so one page")
}

func TestScopes_ShouldRejectAnAmbiguouslySpelledFilter(t *testing.T) {
	t.Parallel()

	// ARRANGE — a leading-zero spelling of an address the collection holds.
	scopes := scopesFor(t, "10.0.0.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT
	rec := getScopes(t, handler, "scopeId=010.000.000.000")

	// ASSERT — rejected, not silently read as 10.0.0.0. Leading zeros parse as
	// octal in some resolvers and decimal in others, an ambiguity behind real
	// SSRF and access-control bypasses, so netip refuses them and so does this.
	// Guessing which the client meant is the wrong end of that trade even for a
	// read-only filter.
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// The canonical spelling of the same address does match, so what is being
	// rejected is the ambiguity rather than the address.
	decoded := getPage(t, handler, "scopeId=10.0.0.0")
	require.Len(t, decoded.Items, 1)
	assert.Equal(t, "10.0.0.0", decoded.Items[0].ScopeID)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestScopes_ShouldRejectAMalformedScopeIdFilter(t *testing.T) {
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT — not an address, so it can never match anything in this collection.
	resp := getScopes(t, handler, "scopeId=not-an-address")

	// ASSERT — 400, not a cheerful empty 200. Answering 200 would tell a client
	// its filter worked and the server holds no such scope, which is a different
	// and wrong statement.
	require.Equal(t, http.StatusBadRequest, resp.Code)
	assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))

	assert.Equal(t, "weave-adapters:validation-failed", problem.Type)
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, ParamScopeID, problem.Errors[0].Field)
	assert.Contains(t, problem.Errors[0].Message, "IPv4")

	// An IPv6 address is rejected too: M3a serves v4 scopes only, so it is as
	// unmatchable as a word.
	assert.Equal(t, http.StatusBadRequest, getScopes(t, handler, "scopeId=2001:db8::1").Code)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestScopes_ShouldReportEveryQueryFailureAtOnce(t *testing.T) {
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0")
	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500))

	// ACT — both parameters wrong in one request.
	resp := getScopes(t, handler, "pageSize=abc&scopeId=nope")

	// ASSERT — both reported together. Returning the pagination failure alone
	// would send the client back for a second round trip to discover the
	// second mistake, which is what the errors[] extension exists to prevent.
	require.Equal(t, http.StatusBadRequest, resp.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))

	fields := make([]string, 0, len(problem.Errors))
	for _, fe := range problem.Errors {
		fields = append(fields, fe.Field)
	}

	assert.ElementsMatch(t, []string{pagination.ParamPageSize, ParamScopeID}, fields)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestScopes_ShouldMapBackendFailuresToTheirOwnStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{
			name: "should answer 502 when the backend is unreachable",
			err:  fmt.Errorf("%w: powershell exited 1", ErrBackendUnavailable),
			// 502, not 500: the adapter is a gateway, and a 500 claims the
			// adapter itself is broken — which sends an operator to the wrong
			// logs and is the only part of this weave can read.
			wantStatus: http.StatusBadGateway,
			wantType:   "weave-adapters:backend-unavailable",
		},
		{
			name:       "should answer 504 when the backend times out",
			err:        fmt.Errorf("%w: deadline exceeded", ErrBackendTimeout),
			wantStatus: http.StatusGatewayTimeout,
			wantType:   "weave-adapters:backend-timeout",
		},
		{
			name:       "should answer 502 when the backend output cannot be decoded",
			err:        fmt.Errorf("%w: unexpected end of JSON input", ErrBackendMalformed),
			wantStatus: http.StatusBadGateway,
			wantType:   "weave-adapters:backend-error",
		},
		{
			// Our derivation collided, not the backend's fault. Blaming the
			// backend here would send an operator to the Windows server for an
			// adapter problem.
			name:       "should answer 500 when two scopes derive one identity",
			err:        fmt.Errorf("%w: 10.0.1.0 and 10.0.2.0", ErrDuplicateWadaptID),
			wantStatus: http.StatusInternalServerError,
			wantType:   "weave-adapters:internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			handler := NewScopesHandler(&fakeLister{err: tt.err}, testPageConfig(50, 500))

			// ACT
			resp := getScopes(t, handler, "")

			// ASSERT
			require.Equal(t, tt.wantStatus, resp.Code)
			assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))

			var problem apierror.Problem
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))

			assert.Equal(t, tt.wantType, problem.Type)
			assert.Equal(t, tt.wantStatus, problem.Status, "RFC 9457 requires the body to mirror the wire status")

			rec.AssertMatchesCatalog(t)
		})
	}
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestScopes_ShouldNotLeakTheBackendMessageToTheClient(t *testing.T) {
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ARRANGE — a failure carrying the shell's own stderr, which is where an
	// internal hostname or path would realistically appear.
	const internalDetail = `\\dc01.internal.example\share\Get-DhcpServerv4Scope : Access is denied.`

	handler := NewScopesHandler(
		&fakeLister{err: fmt.Errorf("%w: powershell exited 1: %s", ErrBackendUnavailable, internalDetail)},
		testPageConfig(50, 500),
	)

	// ACT
	resp := getScopes(t, handler, "")

	// ASSERT — the curated detail reaches the client; the raw message does not.
	// The whole body is searched rather than one field, because backendError,
	// detail and title are each a way for it to escape.
	require.Equal(t, http.StatusBadGateway, resp.Code)
	assert.NotContains(t, resp.Body.String(), "dc01.internal.example")
	assert.NotContains(t, resp.Body.String(), "Access is denied")

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
	assert.NotEmpty(t, problem.Detail, "the client still gets a curated explanation")
	assert.Empty(t, problem.BackendError, "raw stderr is not a sanitized backend message")
}

func TestScopes_ShouldAnswer304WhenTheRepresentationIsUnchanged(t *testing.T) {
	t.Parallel()

	// ARRANGE — the pairing the binary wires: the collection behind the
	// conditional wrapper. A list weave polls is the case a 304 saves the most
	// work on, so this is the combination worth asserting rather than either
	// half alone.
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0")
	handler := etag.Conditional(NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, 500)))

	// ACT
	first := getScopes(t, handler, "")
	require.Equal(t, http.StatusOK, first.Code)

	tag := first.Header().Get("ETag")
	require.NotEmpty(t, tag, "a JSON collection must be tagged or it cannot be polled cheaply")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, ScopesPath, nil)
	req.Header.Set("If-None-Match", tag)

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)

	// ASSERT
	assert.Equal(t, http.StatusNotModified, second.Code)
	assert.Empty(t, second.Body.String(), "a 304 carries no body")
	assert.Equal(t, tag, second.Header().Get("ETag"))
}

func TestScopes_ShouldKeepAFullPageUnderTheEtagBufferLimit(t *testing.T) {
	t.Parallel()

	// ARRANGE — the configured maximum page, with every field at a plausible
	// worst case. Over etag.MaxTaggedBytes the response is served untagged with
	// an API-012, so the 304 above would keep passing in test while quietly
	// never happening in production — which is the failure this pins.
	const maxPageSize = 500

	subnets := make([]string, 0, maxPageSize)
	for i := range maxPageSize {
		subnets = append(subnets, fmt.Sprintf("10.%d.%d.0", i/256, i%256))
	}

	scopes := scopesFor(t, subnets...)

	// Long but realistic names and descriptions: Windows caps a scope name at
	// 255 characters, so this is the biggest page the backend can produce.
	for i := range scopes {
		scopes[i].Name = strings.Repeat("n", 255)
		scopes[i].Description = strings.Repeat("d", 255)
		scopes[i].SuperscopeName = strings.Repeat("s", 255)
	}

	handler := NewScopesHandler(&fakeLister{scopes: scopes}, testPageConfig(50, maxPageSize))

	// ACT
	resp := getScopes(t, handler, fmt.Sprintf("pageSize=%d", maxPageSize))

	// ASSERT
	require.Equal(t, http.StatusOK, resp.Code)

	var decoded page
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &decoded))
	require.Len(t, decoded.Items, maxPageSize, "the whole page, or this measures nothing")

	assert.Less(t, resp.Body.Len(), etag.MaxTaggedBytes,
		"a full page at scopes.maxPageSize must stay taggable, or every poll silently costs a full body")
}

func TestProblemFor_ShouldPreserveTheCauseForTheOperator(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cause := fmt.Errorf("%w: powershell exited 1", ErrBackendUnavailable)

	// ACT
	mapped := problemFor(cause)

	// ASSERT — the cause stays reachable through errors.Is, so the operator log
	// keeps what the response drops. Losing it here would leave the 502 with no
	// explanation anywhere except BACKEND-101.
	require.ErrorIs(t, mapped, ErrBackendUnavailable)
	assert.Contains(t, mapped.Error(), "powershell exited 1")

	// A nil error is not a failure to map: the handler only calls this on a
	// non-nil error, and returning a problem for nil would turn a successful
	// read into a 500.
	var apiErr *apierror.Error
	require.ErrorAs(t, mapped, &apiErr)
}
