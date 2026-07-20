/*
Testing: scope_item.go

Pending:

Tested:

	ScopeHandler.get
	  - TestScopeItem_ShouldServeTheScopeMatchingTheWadaptId: the case the route
	    exists for, including that it serves the bare scope rather than a page
	    envelope.
	  - TestScopeItem_ShouldResolveTheLocationACreateReturns: the coupling that
	    made this route ship with POST rather than after it — a Location built by
	    the create handler is fetchable here.
	  - TestScopeItem_ShouldAnswer404ForAnUnknownWadaptId: including that the
	    identifier is echoed, since a 404 that does not say what was missing is
	    unactionable mid-reconcile.
	  - TestScopeItem_ShouldAnswer404ForAMalformedWadaptId: no format validation
	    on the way in; anything matching nothing is a 404, whatever it looked
	    like. Asserting this pins the decision not to leak the ID's construction
	    through a 400.
	  - TestScopeItem_ShouldNotMatchOnScopeId: the path key is the identity, and
	    a subnet passed where a wadaptId belongs must not resolve.
	  - TestScopeItem_ShouldReadTheBackendOnce: list-and-scan is one spawn, not
	    one per scope.
	  - TestScopeItem_ShouldMapBackendFailuresToTheirOwnStatusCodes: the item
	    route reaches the same backend as the collection and must not collapse
	    the distinction weave reads.

Tested elsewhere:

	problemFor's mapping and cause preservation: scopes_test.go, which owns it.
	wadaptID derivation: identity_test.go. ETag and 304: scopes_test.go — the
	wrapper is applied identically by the binary and is not this file's to
	re-prove.

Declined:

	A benchmark of the linear scan against a binary search. The listing arrives
	sorted, so a search is available, but at DHCP scale (hundreds of scopes at
	the outside) the two are indistinguishable next to the PowerShell spawn that
	produced the listing — and the linear scan is the one that stays correct if
	ListScopes ever stops sorting.

Additional Remarks:

	The handler is constructed with NewScopeHandler(backend) and takes only the
	reader. That is deliberate and is asserted by construction rather than by a
	test: scopeLister has no CreateScope, so an item route that grew a write
	would not compile.
*/
package dhcpwindows

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// getScope drives the item handler through a mux, so the path wildcard is
// populated the way it is in the binary. Calling the handler directly would
// leave r.PathValue empty and quietly test nothing.
func getScope(t *testing.T, backend scopeLister, wadaptID string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle("GET "+ScopeItemPath, NewScopeHandler(backend))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, ScopesPath+"/"+wadaptID, nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	return rec
}

func TestScopeItem_ShouldServeTheScopeMatchingTheWadaptId(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0")
	want := scopes[1]

	// ACT
	rec := getScope(t, &fakeLister{scopes: scopes}, want.WadaptID)

	// ASSERT
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got Scope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, want, got)

	// The bare resource, not a page envelope — an item route that answered with
	// items[] would force every client to unwrap a collection of one.
	assert.NotContains(t, rec.Body.String(), "items")
}

func TestScopeItem_ShouldResolveTheLocationACreateReturns(t *testing.T) {
	t.Parallel()

	// ARRANGE — create answers 201 with a Location, and this route must serve
	// it. A 201 pointing at a 404 tells a client the create did not happen.
	created := scopesFor(t, "10.0.30.0")[0]
	creator := NewScopesHandler(
		&fakeLister{createScope: func(ScopeInput) (Scope, error) { return created, nil }},
		testPageConfig(50, 500), testMaxBodyBytes,
	)

	post := postScope(t, creator, mustJSON(t, validInput()))
	require.Equal(t, http.StatusCreated, post.Code)

	location := post.Header().Get("Location")
	require.NotEmpty(t, location)

	// ACT — fetch exactly the URL the create handed back.
	mux := http.NewServeMux()
	mux.Handle("GET "+ScopeItemPath, NewScopeHandler(&fakeLister{scopes: []Scope{created}}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, location, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// ASSERT
	require.Equal(t, http.StatusOK, rec.Code, "Location %q did not resolve", location)

	var got Scope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, created.WadaptID, got.WadaptID)
}

func TestScopeItem_ShouldAnswer404ForAnUnknownWadaptId(t *testing.T) {
	t.Parallel()

	// ARRANGE — a well-formed identity that this server does not hold.
	scopes := scopesFor(t, "10.0.1.0")
	absent := scopesFor(t, "10.0.99.0")[0].WadaptID

	// ACT
	rec := getScope(t, &fakeLister{scopes: scopes}, absent)

	// ASSERT
	require.Equal(t, http.StatusNotFound, rec.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
	assert.Contains(t, problem.Detail, absent, "the 404 must name what was not found")
}

func TestScopeItem_ShouldAnswer404ForAMalformedWadaptId(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — nothing about the shape of a wadaptId is validated.
	rec := getScope(t, &fakeLister{scopes: scopesFor(t, "10.0.1.0")}, "not-an-identity")

	// ASSERT — 404 rather than 400. A wadaptId is opaque by design, so rejecting
	// a "malformed" one would leak how it is constructed for no gain: anything
	// that matches nothing is absent.
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScopeItem_ShouldNotMatchOnScopeId(t *testing.T) {
	t.Parallel()

	// ARRANGE — the subnet is a filter on the collection, never the item key.
	scopes := scopesFor(t, "10.0.1.0")

	// ACT — pass the scopeId where a wadaptId belongs.
	rec := getScope(t, &fakeLister{scopes: scopes}, scopes[0].ScopeID)

	// ASSERT — 404. Private addresses repeat across installations, so matching
	// them here would make one fleet-wide identifier resolve to different
	// resources on different servers.
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScopeItem_ShouldReadTheBackendOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE
	scopes := scopesFor(t, "10.0.1.0", "10.0.2.0", "10.0.3.0", "10.0.4.0")
	backend := &fakeLister{scopes: scopes}

	// ACT
	rec := getScope(t, backend, scopes[3].WadaptID)

	// ASSERT — one PowerShell spawn per request, whatever the collection size.
	// A scan that re-read per candidate would be invisible in the response and
	// ruinous on a real server.
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, backend.calls)
}

func TestScopeItem_ShouldMapBackendFailuresToTheirOwnStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{"unavailable", ErrBackendUnavailable, http.StatusBadGateway, "weave-adapters:backend-unavailable"},
		{"timeout", ErrBackendTimeout, http.StatusGatewayTimeout, "weave-adapters:backend-timeout"},
		{"malformed", ErrBackendMalformed, http.StatusBadGateway, "weave-adapters:backend-error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			backend := &fakeLister{err: fmt.Errorf("%w: powershell exited 1", tc.err)}

			// ACT
			rec := getScope(t, backend, "anything")

			// ASSERT — a backend outage on the item route must not read as a 404,
			// which would tell a reconciling client the scope had been deleted.
			require.Equal(t, tc.wantStatus, rec.Code)

			var problem apierror.Problem
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			assert.Equal(t, tc.wantType, problem.Type)
		})
	}
}
