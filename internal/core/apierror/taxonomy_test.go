/*
Testing: taxonomy.go

Pending:

Tested:
  taxonomy / lookup
    - TestTaxonomy_ShouldCoverEveryRegisteredResponseCode: no catalog entry names a code the taxonomy lacks.
    - TestTaxonomy_ShouldMapEachCodeToItsHTTPStatus: the full code -> status/title table.
    - TestLookup_ShouldFallBackToInternalForUnknownCode: an unknown code answers 500, never status 0.
  TypeFor / TitleFor
    - TestTypeFor_ShouldNamespaceTheCode: types are prefixed so clients can tell them apart.
  constructors
    - TestConstructors_ShouldBindTheCatalogEventAndStatus: every constructor lands on its event and status.
    - TestValidationFailed_ShouldCarryEveryFieldError: all failures ride one response.
    - TestInternal_ShouldWrapCauseForErrorsIs: errors.Is reaches the wrapped cause.
    - TestInternal_ShouldTolerateNilCause: a nil cause does not panic.

Tested elsewhere:
  How each constructor renders into a body is covered in write_test.go.

Declined:

Additional Remarks:
  TestTaxonomy_ShouldCoverEveryRegisteredResponseCode walks the live registry
  rather than a fixture. It is the guard against catalog drift: adding an event
  with a new ResponseCode and forgetting the taxonomy entry would otherwise ship
  a 500 for what should be a 4xx, and only in production.
*/

package apierror

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

func TestTaxonomy_ShouldCoverEveryRegisteredResponseCode(t *testing.T) {
	t.Parallel()

	// ARRANGE — every event the catalog has registered.
	all := events.GetAll()
	require.NotEmpty(t, all, "the catalog should be registered via init")

	// ACT / ASSERT — every declared response code must have HTTP meaning.
	checked := 0

	for id, spec := range all {
		if spec.ResponseCode == "" {
			continue
		}

		checked++

		_, known := taxonomy[spec.ResponseCode]
		assert.True(t, known,
			"event %s declares response code %q, which the taxonomy does not map; add it to taxonomy.go",
			id, spec.ResponseCode)
	}

	assert.Positive(t, checked, "expected at least one event to declare a response code")
}

func TestTaxonomy_ShouldMapEachCodeToItsHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		code       events.ResponseCode
		wantStatus int
		wantTitle  string
	}{
		{name: "should map validation failures to 400", code: events.CodeValidationFailed, wantStatus: http.StatusBadRequest, wantTitle: "Validation failed"},
		{name: "should map unauthorized to 401", code: events.CodeUnauthorized, wantStatus: http.StatusUnauthorized, wantTitle: "Unauthorized"},
		{name: "should map not found to 404", code: events.CodeNotFound, wantStatus: http.StatusNotFound, wantTitle: "Not found"},
		{name: "should map conflict to 409", code: events.CodeConflict, wantStatus: http.StatusConflict, wantTitle: "Conflict"},
		{name: "should map precondition failed to 412", code: events.CodePreconditionFailed, wantStatus: http.StatusPreconditionFailed, wantTitle: "Precondition failed"},
		{name: "should map backend unreachable to 502", code: events.CodeBackendUnreachable, wantStatus: http.StatusBadGateway, wantTitle: "Backend unreachable"},
		{name: "should map backend error to 502", code: events.CodeBackendError, wantStatus: http.StatusBadGateway, wantTitle: "Backend error"},
		{name: "should map backend timeout to 504", code: events.CodeBackendTimeout, wantStatus: http.StatusGatewayTimeout, wantTitle: "Backend timeout"},
		{name: "should map internal to 500", code: events.CodeInternal, wantStatus: http.StatusInternalServerError, wantTitle: "Internal server error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			got := lookup(tt.code)

			// ASSERT
			assert.Equal(t, tt.wantStatus, got.status)
			assert.Equal(t, tt.wantTitle, got.title)
		})
	}
}

func TestLookup_ShouldFallBackToInternalForUnknownCode(t *testing.T) {
	t.Parallel()

	// ACT — a code no taxonomy entry covers.
	got := lookup(events.ResponseCode("invented-by-an-adapter"))

	// ASSERT — 500 is wrong but serviceable; status 0 would be unserveable.
	assert.Equal(t, http.StatusInternalServerError, got.status)
}

func TestTypeFor_ShouldNamespaceTheCode(t *testing.T) {
	t.Parallel()

	// ACT / ASSERT
	assert.Equal(t, "weave-adapters:not-found", TypeFor(events.CodeNotFound))
	assert.Equal(t, "Not found", TitleFor(events.CodeNotFound))
}

func TestConstructors_ShouldBindTheCatalogEventAndStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        *Error
		wantEvent  events.EventID
		wantStatus int
	}{
		{name: "should bind validation failures", err: ValidationFailed(), wantEvent: catalog.API900, wantStatus: http.StatusBadRequest},
		{name: "should bind unauthorized", err: Unauthorized("token required"), wantEvent: catalog.API901, wantStatus: http.StatusUnauthorized},
		{name: "should bind not found", err: NotFound("lease 10.0.0.5"), wantEvent: catalog.API902, wantStatus: http.StatusNotFound},
		{name: "should bind conflict", err: Conflict("scope is locked"), wantEvent: catalog.API903, wantStatus: http.StatusConflict},
		{name: "should bind precondition failed", err: PreconditionFailed(`"v1"`), wantEvent: catalog.API904, wantStatus: http.StatusPreconditionFailed},
		{name: "should bind backend unreachable", err: BackendUnreachable("dhcp"), wantEvent: catalog.API905, wantStatus: http.StatusBadGateway},
		{name: "should bind backend error", err: BackendError("dhcp"), wantEvent: catalog.API906, wantStatus: http.StatusBadGateway},
		{name: "should bind backend timeout", err: BackendTimeout("dhcp"), wantEvent: catalog.API907, wantStatus: http.StatusGatewayTimeout},
		{name: "should bind internal", err: Internal(errors.New("boom")), wantEvent: catalog.API908, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ASSERT — the constructor binds an event...
			assert.Equal(t, tt.wantEvent, tt.err.EventID())

			// ...and that event resolves to the expected HTTP status.
			spec, ok := events.Get(tt.err.EventID())
			require.True(t, ok, "constructor bound an unregistered event")
			assert.Equal(t, tt.wantStatus, lookup(spec.ResponseCode).status)
		})
	}
}

func TestValidationFailed_ShouldCarryEveryFieldError(t *testing.T) {
	t.Parallel()

	// ARRANGE — clients must be able to fix everything in one round trip.
	failures := []FieldError{
		{Field: "scopeId", Message: "is required"},
		{Field: "leaseDurationSeconds", Message: "must be positive"},
	}

	// ACT
	err := ValidationFailed(failures...)

	// ASSERT
	assert.Equal(t, failures, err.fieldErrors)
	assert.Equal(t, 2, err.fields["fieldErrors"])
}

func TestInternal_ShouldWrapCauseForErrorsIs(t *testing.T) {
	t.Parallel()

	// ARRANGE
	sentinel := errors.New("connection reset")

	// ACT
	err := Internal(sentinel)

	// ASSERT — callers can still inspect the cause even though clients cannot.
	require.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "connection reset")
}

func TestInternal_ShouldTolerateNilCause(t *testing.T) {
	t.Parallel()

	// ACT — a nil cause reaches here whenever a handler returns a typed nil.
	err := Internal(nil)

	// ASSERT
	require.NotNil(t, err)
	assert.Equal(t, catalog.API908, err.EventID())
	assert.Empty(t, err.fields["error"])
}
