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
  New
    - TestNew_ShouldPanicWhenTheEventDeclaresNoResponse: a non-response event is a wiring bug, not a body.
  constructors
    - TestConstructors_ShouldBindTheCatalogEventAndStatus: every constructor lands on its event and status.
    - TestValidation_ShouldCarryEveryFailureAndNameThemForTheOperator: all field failures reach the client; the event names them.
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
		{name: "should map not found to 404", code: events.CodeNotFound, wantStatus: http.StatusNotFound, wantTitle: "Not found"},
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

func TestNew_ShouldPanicWhenTheEventDeclaresNoResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   events.EventID
	}{
		{name: "should panic when the event is a lifecycle event", id: catalog.SYS001},
		{name: "should panic when the event is not registered at all", id: events.EventID("API-999")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT — better a loud panic on the first request down the
			// path than a problem+json with a bare type and an empty detail.
			assert.PanicsWithValue(t,
				"apierror: event "+string(tt.id)+" declares no ResponseCode/ResponseDetail, so it cannot be an API error",
				func() { _ = New(tt.id) },
			)
		})
	}
}

func TestConstructors_ShouldBindTheCatalogEventAndStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        *Error
		wantEvent  events.EventID
		wantStatus int
	}{
		{name: "should bind not found", err: NotFound("lease 10.0.0.5"), wantEvent: catalog.API900, wantStatus: http.StatusNotFound},
		{
			name:      "should bind validation",
			err:       Validation(FieldError{Field: "pageSize", Message: "must be at least 1"}),
			wantEvent: catalog.API903, wantStatus: http.StatusBadRequest,
		},
		{name: "should bind internal", err: Internal(errors.New("boom")), wantEvent: catalog.API901, wantStatus: http.StatusInternalServerError},
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

func TestValidation_ShouldCarryEveryFailureAndNameThemForTheOperator(t *testing.T) {
	t.Parallel()

	// ARRANGE — a request that got two parameters wrong at once.
	failures := []FieldError{
		{Field: "pageSize", Message: "must be at least 1"},
		{Field: "pageToken", Message: "must be a nextPageToken returned by this endpoint"},
	}

	// ACT
	err := Validation(failures...)

	// ASSERT — the client gets both, so it fixes both in one round trip...
	assert.Equal(t, failures, err.fieldErrors)

	// ...and the log line names the fields without repeating their messages.
	assert.Equal(t, []any{"fields", "pageSize, pageToken"}, err.eventData())
}

func TestValidation_ShouldStayLoggableWithNoFailures(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a degenerate call no current caller makes; pagination
	// guards on len(fieldErrors) > 0. It stays reachable for future callers.
	err := Validation()

	// ASSERT — the catalog declares fields required, so an empty value would
	// make the event's own contract false. A malformed diagnostic must not take
	// down the request it was describing.
	assert.Equal(t, []any{"fields", "(none)"}, err.eventData())
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
	assert.Equal(t, catalog.API901, err.EventID())
	assert.Empty(t, err.fields["error"])
}
