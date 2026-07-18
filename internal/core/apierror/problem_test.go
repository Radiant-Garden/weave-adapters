/*
Testing: problem.go

Pending:

Tested:
  Error.Error
    - TestError_ShouldRenderOperatorFacingMessage: the detail template, resolved.
    - TestError_ShouldAppendCause: the operator form includes the internal cause.
    - TestError_ShouldReportUnregisteredEvent: a bad binding says so instead of rendering blank.
  Error.Unwrap
    - TestUnwrap_ShouldExposeCause: errors.Is/As reach the wrapped error.
  Error.WithCause / WithBackendError / WithFieldErrors
    - TestWithBuilders_ShouldChainAndAccumulate: builders compose; field errors append.
    - TestWithBuilders_ShouldNotMutateReceiver: decorating a shared *Error copies rather than writes.
  Problem
    - TestProblem_ShouldOmitEmptyOptionalFields: absent extensions stay out of the JSON.

Tested elsewhere:
  Rendering a Problem onto the wire is covered in write_test.go; the constructors
  in taxonomy_test.go.

Declined:
  EventID — a one-line accessor, exercised throughout taxonomy_test.go.

Additional Remarks:
  Error.Error is the operator-facing form and deliberately includes the cause.
  The client-facing form is Problem.Detail, which never does — write_test.go
  covers that boundary.
*/

package apierror

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

func TestError_ShouldRenderOperatorFacingMessage(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := NotFound("lease 10.0.0.5")

	// ASSERT
	assert.Equal(t, "The requested lease 10.0.0.5 was not found.", err.Error())
}

func TestError_ShouldAppendCause(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := NotFound("lease 10.0.0.5").WithCause(errors.New("dial tcp: i/o timeout"))

	// ASSERT — operators need the cause; clients never see this string.
	assert.Equal(t, "The requested lease 10.0.0.5 was not found.: dial tcp: i/o timeout", err.Error())
}

func TestError_ShouldReportUnregisteredEvent(t *testing.T) {
	t.Parallel()

	// ARRANGE — a constructor bound to an ID nobody registered.
	err := newError(events.EventID("API-999"), nil)

	// ACT / ASSERT — naming the broken binding beats rendering an empty string.
	assert.Contains(t, err.Error(), "API-999")
}

func TestUnwrap_ShouldExposeCause(t *testing.T) {
	t.Parallel()

	// ARRANGE
	sentinel := errors.New("connection reset")

	// ACT
	err := NotFound("lease 10.0.0.5").WithCause(sentinel)

	// ASSERT
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, sentinel, err.Unwrap())
}

func TestWithBuilders_ShouldChainAndAccumulate(t *testing.T) {
	t.Parallel()

	// ARRANGE
	cause := errors.New("boom")

	// ACT — builders chain, and field errors accumulate across calls.
	err := NotFound("lease").
		WithFieldErrors(FieldError{Field: "a", Message: "is required"}).
		WithFieldErrors(FieldError{Field: "b", Message: "must be positive"}).
		WithBackendError("sanitized backend text").
		WithCause(cause)

	// ASSERT
	require.Len(t, err.fieldErrors, 2)
	assert.Equal(t, "b", err.fieldErrors[1].Field)
	assert.Equal(t, "sanitized backend text", err.backendError)
	assert.Equal(t, cause, err.Unwrap())
}

func TestWithBuilders_ShouldNotMutateReceiver(t *testing.T) {
	t.Parallel()

	// ARRANGE — the shape a handler package would reach for: one shared error
	// value decorated per request.
	shared := NotFound("lease")

	// ACT
	first := shared.WithCause(errors.New("first"))
	second := shared.WithFieldErrors(FieldError{Field: "b", Message: "bad"})

	// ASSERT — decorating must not write through to the shared value, or two
	// concurrent requests would race on it and leak each other's detail.
	require.NoError(t, shared.Unwrap())
	assert.Empty(t, shared.fieldErrors)
	assert.Empty(t, first.fieldErrors)
	require.NoError(t, second.Unwrap())
	assert.Equal(t, "first", first.Unwrap().Error())
	assert.Len(t, second.fieldErrors, 1)
}

func TestProblem_ShouldOmitEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	// ARRANGE — a minimal problem, as a 404 from middleware would produce.
	problem := Problem{
		Type:   TypeFor(events.CodeNotFound),
		Title:  TitleFor(events.CodeNotFound),
		Status: http.StatusNotFound,
	}

	// ACT
	raw, err := json.Marshal(problem)
	require.NoError(t, err)

	// ASSERT — absent extensions must not appear as nulls or empty strings.
	assert.NotContains(t, string(raw), "detail")
	assert.NotContains(t, string(raw), "instance")
	assert.NotContains(t, string(raw), "requestId")
	assert.NotContains(t, string(raw), "backendError")
	assert.NotContains(t, string(raw), "errors")
	assert.Contains(t, string(raw), `"status":404`)
}
