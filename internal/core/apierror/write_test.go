/*
Testing: write.go

Pending:

Tested:
  WriteError
    - TestWriteError_ShouldRenderProblemJSON: status, content type, and every body field.
    - TestWriteError_ShouldEmitExactlyOneEvent: one call, one log line.
    - TestWriteError_ShouldRedactUnmappedErrors: an internal message never reaches the client.
    - TestWriteError_ShouldResolveWrappedAPIErrors: fmt.Errorf("%w") keeps the taxonomy entry.
    - TestWriteError_ShouldIncludeRequestIDFromContext: responses correlate with logs.
    - TestWriteError_ShouldRenderValidationFieldErrors: errors[] reaches the body.
    - TestWriteError_ShouldRenderBackendError: the sanitized backend message is surfaced.
  WriteProblem
    - TestWriteProblem_ShouldWriteWithoutEmitting: the recovery path stays single-lined.
  render
    - TestRender_ShouldSubstitutePlaceholders: {{key}} substitution.
    - TestRender_ShouldLeaveUnresolvedPlaceholdersVisible: a missing field is loud, not blank.
  eventData
    - TestEventData_ShouldOrderFieldsDeterministically: stable key order across calls.

Tested elsewhere:
  Code-to-status mapping is covered in taxonomy_test.go.

Declined:
  asError / problem / valueToString — unexported helpers, asserted through
  WriteError's observable output rather than called directly.

Additional Remarks:
  Tests that install the event recorder mutate the process-global emitter hook
  and therefore cannot run in parallel.

  Requests are built with a caller context, since the API-9xx events are
  ExternalSource and Emit panics without a remoteAddr — the same guard the
  request-ID middleware satisfies in production.
*/

package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
)

// newRequest builds a request carrying the caller context the request-ID
// middleware would have populated.
func newRequest(t *testing.T, path string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	ctx := events.WithCaller(req.Context(), events.Caller{
		RemoteAddr: "192.0.2.1:1234",
		RequestID:  "req-abc",
		Method:     http.MethodGet,
		Path:       path,
	})

	return req.WithContext(ctx)
}

// decodeProblem reads a problem body off a recorder.
func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) Problem {
	t.Helper()

	var problem Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))

	return problem
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldRenderProblemJSON(t *testing.T) {
	// ARRANGE
	events := eventstest.NewRecorder()
	t.Cleanup(events.Install())

	rec := httptest.NewRecorder()
	req := newRequest(t, "/api/v1/leases/10.0.0.5")

	// ACT
	WriteError(rec, req, NotFound("lease 10.0.0.5"))

	// ASSERT
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))

	problem := decodeProblem(t, rec)
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
	assert.Equal(t, "Not found", problem.Title)
	assert.Equal(t, http.StatusNotFound, problem.Status)
	assert.Equal(t, "The requested lease 10.0.0.5 was not found.", problem.Detail)
	assert.Equal(t, "/api/v1/leases/10.0.0.5", problem.Instance)
	assert.Equal(t, "req-abc", problem.RequestID)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldEmitExactlyOneEvent(t *testing.T) {
	// ARRANGE — the single-handling rule: one failure, one log line.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	WriteError(httptest.NewRecorder(), newRequest(t, "/api/v1/leases"), BackendTimeout("dhcp"))

	// ASSERT
	rec.AssertEmittedN(t, catalog.API907, 1)
	rec.AssertData(t, catalog.API907, "backend", "dhcp")
	rec.AssertMatchesCatalog(t)
	assert.Len(t, rec.All(), 1, "WriteError should emit the error event and nothing else")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldRedactUnmappedErrors(t *testing.T) {
	// ARRANGE — an error that never passed through the taxonomy.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	leaky := errors.New("dial tcp 10.9.9.9:445: connect: connection refused")

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), leaky)

	// ASSERT — the client learns nothing about internal topology...
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "10.9.9.9")
	assert.NotContains(t, recorder.Body.String(), "connection refused")

	problem := decodeProblem(t, recorder)
	assert.Equal(t, "An unexpected error occurred.", problem.Detail)

	// ...while the operator gets the full cause.
	rec.AssertEmitted(t, catalog.API908)
	rec.AssertData(t, catalog.API908, "error", leaky.Error())
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldResolveWrappedAPIErrors(t *testing.T) {
	// ARRANGE — handlers wrap as errors travel up the stack.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	wrapped := fmt.Errorf("loading lease: %w", NotFound("lease 10.0.0.5"))

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases/10.0.0.5"), wrapped)

	// ASSERT — wrapping must not degrade a 404 into a 500.
	assert.Equal(t, http.StatusNotFound, recorder.Code)
	rec.AssertEmitted(t, catalog.API902)
	rec.AssertNotEmitted(t, catalog.API908)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldIncludeRequestIDFromContext(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), Unauthorized("a bearer token is required"))

	// ASSERT — the ID in the body is what an operator greps for in the logs.
	problem := decodeProblem(t, recorder)
	assert.Equal(t, "req-abc", problem.RequestID)
	assert.Equal(t, "a bearer token is required", problem.Detail)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldRenderValidationFieldErrors(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	err := ValidationFailed(
		FieldError{Field: "scopeId", Message: "is required"},
		FieldError{Field: "leaseDurationSeconds", Message: "must be positive"},
	)

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), err)

	// ASSERT — every failure in one response, not first-failure-only.
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	problem := decodeProblem(t, recorder)
	require.Len(t, problem.Errors, 2)
	assert.Equal(t, "scopeId", problem.Errors[0].Field)
	assert.Equal(t, "must be positive", problem.Errors[1].Message)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldRenderBackendError(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	err := BackendError("dhcp").WithBackendError("The DHCP server rejected the scope ID.")

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/scopes"), err)

	// ASSERT
	require.Equal(t, http.StatusBadGateway, recorder.Code)

	problem := decodeProblem(t, recorder)
	assert.Equal(t, "The dhcp backend reported an error.", problem.Detail)
	assert.Equal(t, "The DHCP server rejected the scope ID.", problem.BackendError)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteProblem_ShouldWriteWithoutEmitting(t *testing.T) {
	// ARRANGE — the recovery middleware already emitted API-011 for this panic.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	recorder := httptest.NewRecorder()

	// ACT
	WriteProblem(recorder, Problem{
		Type:   TypeFor(events.CodeInternal),
		Title:  TitleFor(events.CodeInternal),
		Status: http.StatusInternalServerError,
		Detail: "An unexpected error occurred.",
	})

	// ASSERT — a second event here would make one panic look like two failures.
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, "application/problem+json", recorder.Header().Get("Content-Type"))
	assert.Empty(t, rec.All(), "WriteProblem must not emit")
}

func TestRender_ShouldSubstitutePlaceholders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		fields   map[string]any
		want     string
	}{
		{
			name:     "should substitute a single placeholder",
			template: "The requested {{resource}} was not found.",
			fields:   map[string]any{"resource": "lease 10.0.0.5"},
			want:     "The requested lease 10.0.0.5 was not found.",
		},
		{
			name:     "should substitute several placeholders",
			template: "{{a}} then {{b}}",
			fields:   map[string]any{"a": "first", "b": "second"},
			want:     "first then second",
		},
		{
			name:     "should render non-string values",
			template: "{{count}} fields failed",
			fields:   map[string]any{"count": 3},
			want:     "3 fields failed",
		},
		{
			name:     "should pass through a template with no placeholders",
			template: "The request failed validation.",
			fields:   map[string]any{"unused": "x"},
			want:     "The request failed validation.",
		},
		{
			name:     "should pass through when there are no fields",
			template: "The requested {{resource}} was not found.",
			fields:   nil,
			want:     "The requested {{resource}} was not found.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT
			assert.Equal(t, tt.want, render(tt.template, tt.fields))
		})
	}
}

func TestRender_ShouldLeaveUnresolvedPlaceholdersVisible(t *testing.T) {
	t.Parallel()

	// ACT — a field the caller forgot to supply.
	got := render("The {{backend}} backend timed out after {{seconds}}s.", map[string]any{"backend": "dhcp"})

	// ASSERT — visibly broken beats silently blank; "after s." reads as cosmetic
	// and would survive review, whereas the raw placeholder is a bug report.
	assert.Equal(t, "The dhcp backend timed out after {{seconds}}s.", got)
}

func TestEventData_ShouldOrderFieldsDeterministically(t *testing.T) {
	t.Parallel()

	// ARRANGE — Go randomizes map iteration, so unsorted output would vary.
	err := &Error{fields: map[string]any{"zulu": 1, "alpha": 2, "mike": 3}}

	// ACT
	first, second := err.eventData(), err.eventData()

	// ASSERT
	assert.Equal(t, []any{"alpha", 2, "mike", 3, "zulu", 1}, first)
	assert.Equal(t, first, second)
}
