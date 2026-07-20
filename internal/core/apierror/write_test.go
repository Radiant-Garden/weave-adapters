/*
Testing: write.go

Pending:

Tested:
  WriteError
    - TestWriteError_ShouldRenderProblemJSON: status, content type, and every body field.
    - TestWriteError_ShouldEmitExactlyOneEvent: one call, one log line.
    - TestWriteError_ShouldEmitOneEventForAValidationFailure: many field failures, still one API-903 line.
    - TestWriteError_ShouldRedactUnmappedErrors: an internal message never reaches the client.
    - TestWriteError_ShouldResolveWrappedAPIErrors: fmt.Errorf("%w") keeps the taxonomy entry.
    - TestWriteError_ShouldIncludeRequestIDFromContext: responses correlate with logs.
    - TestWriteError_ShouldRenderFieldErrors: errors[] reaches the body.
    - TestWriteError_ShouldRenderBackendError: the sanitized backend message is surfaced, the cause is not.
    - TestWriteError_ShouldNotPanicWithoutCallerContext: a request outside the middleware chain still gets its status.
    - TestWriteError_ShouldStaySilentForACancelledRequest: a request whose context is done gets no response and emits no API-901 — a client hang-up is not an adapter fault.
  TruncatePath
    - TestTruncatePath_ShouldBoundOnARuneBoundary: an overlong multi-byte path is cut without splitting a rune into U+FFFD.
    - TestTruncatePath_ShouldLeaveAShortPathUntouched: a path under the limit passes through verbatim.
  FallbackCaller
    - TestFallbackCaller_ShouldTruncateTheReflectedPath: the shared helper bounds the path the three fallback sites once echoed raw.
  WriteProblem
    - TestWriteProblem_ShouldWriteWithoutEmitting: the recovery path stays single-lined.
    - TestWriteProblem_ShouldDefaultMissingStatus: an unset status answers 500 instead of panicking.
  render
    - TestRender_ShouldSubstitutePlaceholders: {{key}} substitution.
    - TestRender_ShouldLeaveUnresolvedPlaceholdersVisible: a missing field is loud, not blank.
  eventData
    - TestEventData_ShouldOrderFieldsDeterministically: stable key order across calls.

Tested elsewhere:
  Code-to-status mapping is covered in taxonomy_test.go.

Declined:
  asError / problem — unexported helpers, asserted through WriteError's
  observable output rather than called directly.

Additional Remarks:
  Tests that install the event recorder mutate the process-global emitter hook
  and therefore cannot run in parallel.

  Requests are built with a caller context, since the API-9xx events are
  ExternalSource and Emit panics without a remoteAddr — the same guard the
  request-ID middleware satisfies in production.
*/

package apierror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

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
	WriteError(httptest.NewRecorder(), newRequest(t, "/api/v1/leases"), Internal(errors.New("backend timed out")))

	// ASSERT
	rec.AssertEmittedN(t, catalog.API901, 1)
	rec.AssertData(t, catalog.API901, "error", "backend timed out")
	rec.AssertMatchesCatalog(t)
	assert.Len(t, rec.All(), 1, "WriteError should emit the error event and nothing else")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldEmitOneEventForAValidationFailure(t *testing.T) {
	// ARRANGE — several field failures still make one response and one log
	// line; the errors[] extension is what carries the multiplicity.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	err := Validation(
		FieldError{Field: "pageSize", Message: "must be at least 1"},
		FieldError{Field: "pageToken", Message: "must be a nextPageToken returned by this endpoint"},
	)

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), err)

	// ASSERT
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	rec.AssertEmittedN(t, catalog.API903, 1)
	rec.AssertData(t, catalog.API903, "fields", "pageSize, pageToken")
	rec.AssertMatchesCatalog(t)
	assert.Len(t, rec.All(), 1, "two field failures are still one event")
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
	rec.AssertEmitted(t, catalog.API901)
	rec.AssertData(t, catalog.API901, "error", leaky.Error())
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
	rec.AssertEmitted(t, catalog.API900)
	rec.AssertNotEmitted(t, catalog.API901)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldIncludeRequestIDFromContext(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), NotFound("lease 10.0.0.5"))

	// ASSERT — the ID in the body is what an operator greps for in the logs.
	problem := decodeProblem(t, recorder)
	assert.Equal(t, "req-abc", problem.RequestID)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldRenderFieldErrors(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	err := NotFound("lease 10.0.0.5").WithFieldErrors(
		FieldError{Field: "scopeId", Message: "is required"},
		FieldError{Field: "leaseDurationSeconds", Message: "must be positive"},
	)

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/leases"), err)

	// ASSERT — every failure in one response, not first-failure-only.
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

	err := Internal(errors.New("winrm: 500")).WithBackendError("The DHCP server rejected the scope ID.")

	recorder := httptest.NewRecorder()

	// ACT
	WriteError(recorder, newRequest(t, "/api/v1/scopes"), err)

	// ASSERT — the sanitized backend text is surfaced; the raw cause is not.
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	problem := decodeProblem(t, recorder)
	assert.Equal(t, "The DHCP server rejected the scope ID.", problem.BackendError)
	assert.NotContains(t, recorder.Body.String(), "winrm: 500")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldNotPanicWithoutCallerContext(t *testing.T) {
	// ARRANGE — a request that never passed through the request-ID middleware:
	// a route mounted outside the chain, or a handler under direct unit test.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/leases/1", nil)
	recorder := httptest.NewRecorder()

	// ACT — Emit panics on an ExternalSource event with no remoteAddr, so
	// without a fallback this would 500 via the recovery middleware instead of
	// returning the 404 the handler asked for.
	require.NotPanics(t, func() {
		WriteError(recorder, req, NotFound("lease 1"))
	})

	// ASSERT — the intended status survives, and the event still carries an
	// address, seeded from the request.
	assert.Equal(t, http.StatusNotFound, recorder.Code)
	rec.AssertEmitted(t, catalog.API900)
	rec.AssertMatchesCatalog(t)

	emitted := rec.FindByID(catalog.API900)
	require.NotEmpty(t, emitted)
	assert.Equal(t, req.RemoteAddr, emitted[0].Caller("remoteAddr"))
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteError_ShouldStaySilentForACancelledRequest(t *testing.T) {
	// ARRANGE — a request whose own context is done, which is what a client that
	// hung up mid-request produces. There is nobody to answer.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ctx, cancel := context.WithCancel(newRequest(t, "/api/v1/leases/1").Context())
	cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/leases/1", nil)
	recorder := httptest.NewRecorder()

	// ACT — an unmapped context.Canceled would otherwise resolve to API-901
	// "internal error" at ERROR and raise a false alarm on every dropped
	// connection.
	WriteError(recorder, req, context.Canceled)

	// ASSERT — nothing written, nothing emitted. The request is not invisible:
	// the logging middleware's API-010 still records it.
	assert.Empty(t, recorder.Body.String())
	rec.AssertNotEmitted(t, catalog.API901)
}

func TestTruncatePath_ShouldBoundOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// ARRANGE — a path far past the limit whose byte at the cut offset is in the
	// middle of a multi-byte rune. Slicing bytes there would emit U+FFFD into the
	// very field the bound exists to keep clean.
	path := "/api/v1/scopes/" + strings.Repeat("ä", MaxReflectedPathLen)

	// ACT
	got := TruncatePath(path)

	// ASSERT — bounded, marked, and still valid UTF-8: no rune was split.
	assert.LessOrEqual(t, len(got), MaxReflectedPathLen+len("…"))
	assert.True(t, strings.HasSuffix(got, "…"))
	assert.True(t, utf8.ValidString(got), "a rune was split mid-sequence")
}

func TestTruncatePath_ShouldLeaveAShortPathUntouched(t *testing.T) {
	t.Parallel()

	// ARRANGE
	path := "/api/v1/scopes/10.0.0.0"

	// ACT / ASSERT — under the limit passes through verbatim, no marker.
	assert.Equal(t, path, TruncatePath(path))
}

func TestFallbackCaller_ShouldTruncateTheReflectedPath(t *testing.T) {
	t.Parallel()

	// ARRANGE — the three fallback sites built this struct by hand and all passed
	// the path raw; the helper is the one place that bounds it.
	long := "/" + strings.Repeat("a", 2*MaxReflectedPathLen)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, long, nil)
	req.RemoteAddr = "192.0.2.1:1234"

	// ACT
	caller := FallbackCaller(req)

	// ASSERT — bounded here, so no reflection site can reopen the amplifier.
	assert.Equal(t, "192.0.2.1:1234", caller.RemoteAddr)
	assert.Equal(t, http.MethodGet, caller.Method)
	assert.LessOrEqual(t, len(caller.Path), MaxReflectedPathLen+len("…"))
	assert.True(t, strings.HasSuffix(caller.Path, "…"))
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestWriteProblem_ShouldDefaultMissingStatus(t *testing.T) {
	// ARRANGE — a Problem built without a Status, as a caller using TypeFor and
	// TitleFor could easily produce.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	recorder := httptest.NewRecorder()

	// ACT — net/http panics on any code below 100, so an unset status must not
	// reach WriteHeader.
	require.NotPanics(t, func() {
		WriteProblem(recorder, Problem{Type: TypeFor(events.CodeInternal), Title: TitleFor(events.CodeInternal)})
	})

	// ASSERT — the body must mirror the wire status, or the response is not a
	// conforming problem+json: RFC 9457 requires the status member to match.
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var problem Problem

	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&problem))
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
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
