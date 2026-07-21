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

	ScopeHandler.ServeHTTP (method switch)
	  - TestScopeItem_ShouldAnswer405ForAnUnsupportedMethod: the default arm is a
	    405 naming the allowed verbs, never a fall-through to GET.
	ScopeHandler.delete
	  - TestScopeItem_ShouldAnswer204OnDelete: the removal path, no body.
	  - TestScopeItem_ShouldAnswer404WhenDeletingAnUnknownScope: absent is 404, the
	    answer weave treats as already-deleted, and the id is echoed.
	  - TestScopeItem_ShouldMapDeleteBackendFailuresToTheirOwnStatusCodes: a backend
	    fault on delete keeps its 502/504 rather than reading as a 404.
	ScopeHandler.update / updateProblemFor
	  - TestScopeItem_ShouldAnswer200WithTheUpdatedScope: the success path, body
	    carrying the scope as it now exists.
	  - TestScopeItem_ShouldPassTheDecodedUpdateToTheBackend: only the provided
	    fields reach the backend, as pointers.
	  - TestScopeItem_ShouldRejectAnUpdateAssertingADerivedField: sending scopeId is
	    a 400, not a silent drop — the identity cannot be set by a client.
	  - TestScopeItem_ShouldAnswer404WhenUpdatingAnUnknownScope: absent is 404.
	  - TestScopeItem_ShouldAnswer400ForAResizeThatLeavesTheSubnet: an out-of-subnet
	    range is a validation error naming the offending range field.
	  - TestScopeItem_ShouldRejectAnInvalidUpdateBeforeTouchingTheBackend: a bad body
	    is a 400 with the backend never called.

Tested elsewhere:

	problemFor's mapping and cause preservation: scopes_test.go, which owns it.
	ScopeUpdate.Validate and env: update_test.go. DeleteScope/UpdateScope against
	the fake runner: mutate_test.go. wadaptID derivation: identity_test.go. ETag
	and 304: scopes_test.go — the wrapper is applied identically by the binary and
	is not this file's to re-prove. The body-limit, media-type and malformed-JSON
	rejections belong to requestbody; this file asserts only that update delegates
	to it.

Declined:

	A benchmark of the linear scan against a binary search. The listing arrives
	sorted, so a search is available, but at DHCP scale (hundreds of scopes at
	the outside) the two are indistinguishable next to the PowerShell spawn that
	produced the listing — and the linear scan is the one that stays correct if
	ListScopes ever stops sorting.

Additional Remarks:

	The handler now serves GET, DELETE and PATCH, so its backend is
	scopeItemBackend (lister + deleter + updater) rather than the bare reader it
	took when the route was read-only. The collection handler still takes only
	lister + creator, so neither route can reach past its own verbs — asserted by
	construction rather than by a test, since the interfaces would not compile
	otherwise.

	The identity-preserving range check and the two-spawn resolve live on the
	client (mutate.go), so this file drives a fake backend and asserts only the
	handler's own mapping — status codes, the 204/404/405 shapes, and that a
	validation failure names the field. The out-of-subnet case is exercised end to
	end against the real client in mutate_test.go.
*/
package dhcpwindows

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// itemMux mounts the item handler on the three verbs the binary serves, so the
// path wildcard is populated the way it is in production. Calling the handler
// directly would leave r.PathValue empty and quietly test nothing.
func itemMux(backend scopeItemBackend) *http.ServeMux {
	mux := http.NewServeMux()
	h := NewScopeHandler(backend, testMaxBodyBytes)
	mux.Handle("GET "+ScopeItemPath, h)
	mux.Handle("DELETE "+ScopeItemPath, h)
	mux.Handle("PATCH "+ScopeItemPath, h)

	return mux
}

// itemRequest drives the item handler for one method and identity, through the
// mux so r.PathValue is populated.
func itemRequest(t *testing.T, backend scopeItemBackend, method, wadaptID, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, ScopesPath+"/"+wadaptID, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	itemMux(backend).ServeHTTP(rec, req)

	return rec
}

// getScope drives a GET on the item route.
func getScope(t *testing.T, backend scopeItemBackend, wadaptID string) *httptest.ResponseRecorder {
	t.Helper()

	return itemRequest(t, backend, http.MethodGet, wadaptID, "")
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
	mux.Handle("GET "+ScopeItemPath, NewScopeHandler(&fakeLister{scopes: []Scope{created}}, testMaxBodyBytes))

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

func TestScopeItem_ShouldAnswer405ForAnUnsupportedMethod(t *testing.T) {
	t.Parallel()

	// ARRANGE — PUT is not one of the three verbs the item route serves. Driven
	// straight through the handler so its own default arm answers, the way it
	// must when the mux is absent (in the binary the router 405s first).
	handler := NewScopeHandler(&fakeLister{scopes: scopesFor(t, "10.0.1.0")}, testMaxBodyBytes)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, ScopesPath+"/anything", nil)
	rec := httptest.NewRecorder()

	// ACT
	handler.ServeHTTP(rec, req)

	// ASSERT — a 405, never a fall-through that serves the scope for a verb the
	// route does not implement.
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"wadaptId"`, "an unsupported method must not serve the scope")
}

func TestScopeItem_ShouldAnswer204OnDelete(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var deleted string

	backend := &fakeLister{deleteScope: func(wadaptID string) error {
		deleted = wadaptID

		return nil
	}}
	target := scopesFor(t, "10.0.30.0")[0].WadaptID

	// ACT
	rec := itemRequest(t, backend, http.MethodDelete, target, "")

	// ASSERT — 204 with no body, and the path identity reaches the backend.
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "a 204 carries no body")
	assert.Equal(t, target, deleted)
}

func TestScopeItem_ShouldAnswer404WhenDeletingAnUnknownScope(t *testing.T) {
	t.Parallel()

	// ARRANGE — the backend reports the identity names nothing.
	absent := scopesFor(t, "10.0.99.0")[0].WadaptID
	backend := &fakeLister{deleteScope: func(wadaptID string) error {
		return fmt.Errorf("%w: %s", ErrScopeNotFound, wadaptID)
	}}

	// ACT
	rec := itemRequest(t, backend, http.MethodDelete, absent, "")

	// ASSERT — 404, the answer weave treats as already-deleted, and the id is
	// echoed so a reconciling client can tell which delete it was.
	require.Equal(t, http.StatusNotFound, rec.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
	assert.Contains(t, problem.Detail, absent)
}

func TestScopeItem_ShouldMapDeleteBackendFailuresToTheirOwnStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unavailable", ErrBackendUnavailable, http.StatusBadGateway},
		{"timeout", ErrBackendTimeout, http.StatusGatewayTimeout},
		{"malformed", ErrBackendMalformed, http.StatusBadGateway},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			backend := &fakeLister{deleteScope: func(string) error {
				return fmt.Errorf("%w: powershell exited 1", tc.err)
			}}

			// ACT
			rec := itemRequest(t, backend, http.MethodDelete, "anything", "")

			// ASSERT — a backend fault on delete keeps its own code. Collapsing it
			// to a 404 would tell a reconciling client the scope was already gone
			// when the adapter simply could not reach the server.
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestScopeItem_ShouldAnswer200WithTheUpdatedScope(t *testing.T) {
	t.Parallel()

	// ARRANGE
	updated := scopesFor(t, "10.0.30.0")[0]
	updated.Name = "renamed"
	backend := &fakeLister{updateScope: func(string, ScopeUpdate) (Scope, error) { return updated, nil }}

	// ACT
	rec := itemRequest(t, backend, http.MethodPatch, updated.WadaptID, `{"name":"renamed"}`)

	// ASSERT
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got Scope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, updated.WadaptID, got.WadaptID)
	assert.Equal(t, "renamed", got.Name)

	// The bare resource, not a page envelope.
	assert.NotContains(t, rec.Body.String(), "items")
}

func TestScopeItem_ShouldPassTheDecodedUpdateToTheBackend(t *testing.T) {
	t.Parallel()

	// ARRANGE
	updated := scopesFor(t, "10.0.30.0")[0]
	backend := &fakeLister{updateScope: func(string, ScopeUpdate) (Scope, error) { return updated, nil }}

	// ACT
	rec := itemRequest(t, backend, http.MethodPatch, updated.WadaptID,
		`{"name":"renamed","leaseDurationSeconds":3600}`)

	// ASSERT — the provided fields arrive as non-nil pointers; the omitted ones
	// stay nil, which is how the backend knows to leave them unchanged.
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.NotNil(t, backend.updated.Name)
	assert.Equal(t, "renamed", *backend.updated.Name)
	require.NotNil(t, backend.updated.LeaseDurationSeconds)
	assert.Equal(t, 3600, *backend.updated.LeaseDurationSeconds)
	assert.Nil(t, backend.updated.Description, "an omitted field must not reach the backend as a value")
	assert.Nil(t, backend.updated.State)
}

func TestScopeItem_ShouldRejectAnUpdateAssertingADerivedField(t *testing.T) {
	t.Parallel()

	// ARRANGE
	backend := &fakeLister{updateScope: func(string, ScopeUpdate) (Scope, error) {
		return Scope{}, errors.New("the backend must not be reached")
	}}

	// ACT — scopeId is derived and not a field of ScopeUpdate, so
	// DisallowUnknownFields rejects it rather than letting a client believe it
	// moved the scope's identity.
	rec := itemRequest(t, backend, http.MethodPatch, "anyid", `{"name":"x","scopeId":"10.0.0.0"}`)

	// ASSERT
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, backend.calls)
}

func TestScopeItem_ShouldAnswer404WhenUpdatingAnUnknownScope(t *testing.T) {
	t.Parallel()

	// ARRANGE
	absent := scopesFor(t, "10.0.99.0")[0].WadaptID
	backend := &fakeLister{updateScope: func(wadaptID string, _ ScopeUpdate) (Scope, error) {
		return Scope{}, fmt.Errorf("%w: %s", ErrScopeNotFound, wadaptID)
	}}

	// ACT
	rec := itemRequest(t, backend, http.MethodPatch, absent, `{"name":"x"}`)

	// ASSERT — 404, which weave reads as "target gone, re-create next cycle".
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScopeItem_ShouldAnswer400ForAResizeThatLeavesTheSubnet(t *testing.T) {
	t.Parallel()

	// ARRANGE — the client method rejects a range that would move the identity,
	// naming the offending field.
	backend := &fakeLister{updateScope: func(string, ScopeUpdate) (Scope, error) {
		return Scope{}, &rangeOutsideSubnetError{scopeID: "10.0.30.0", fields: []string{"endRange"}}
	}}

	// ACT
	rec := itemRequest(t, backend, http.MethodPatch, "someid", `{"endRange":"10.0.31.250"}`)

	// ASSERT — a 400 naming the field that actually left the subnet, not a
	// hardcoded one, and the subnet reaches the client.
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, "weave-adapters:validation-failed", problem.Type)
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, "endRange", problem.Errors[0].Field)
	assert.Contains(t, problem.Errors[0].Message, "10.0.30.0")
}

func TestScopeItem_ShouldRejectAnInvalidUpdateBeforeTouchingTheBackend(t *testing.T) {
	t.Parallel()

	// ARRANGE
	backend := &fakeLister{updateScope: func(string, ScopeUpdate) (Scope, error) {
		return Scope{}, errors.New("the backend must not be reached")
	}}

	// ACT — a negative lease and an unknown state, both invalid.
	rec := itemRequest(t, backend, http.MethodPatch, "someid",
		`{"leaseDurationSeconds":-1,"state":"Paused"}`)

	// ASSERT — 400 with every failure at once, and the backend never spawned.
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Zero(t, backend.calls)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Len(t, problem.Errors, 2)
}
