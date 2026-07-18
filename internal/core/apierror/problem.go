// Package apierror is the adapter's uniform error model: every client-facing
// failure is an RFC 9457 application/problem+json response backed by a
// cataloged event.
//
// The pairing is the point. WriteError emits the event and renders the body
// from the same catalog entry, so the operator log line and the error weave
// receives cannot drift apart, and every error a client sees is also a
// documented, ID'd log line with caller context.
//
// Handlers return errors; they never log and respond. WriteError is the single
// place that does both.
package apierror

import (
	"fmt"
	"slices"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// ContentType is the RFC 9457 media type for problem responses. It is exported
// so middleware can recognise a response this package already rendered.
const ContentType = "application/problem+json"

// Problem is an RFC 9457 problem detail. Fields beyond the standard set
// (requestId, backendError, errors) are the extensions this API defines.
type Problem struct {
	// Type is a stable slug identifying the error class, e.g.
	// "weave-adapters:not-found". Clients may switch on it.
	Type string `json:"type"`
	// Title is a short, human-readable summary of the error class.
	Title string `json:"title"`
	// Status is the HTTP status code.
	Status int `json:"status"`
	// Detail explains this specific occurrence. It is curated for clients — it
	// never carries an internal error message.
	Detail string `json:"detail,omitempty"`
	// Instance is the request path the error occurred on.
	Instance string `json:"instance,omitempty"`
	// RequestID correlates the response with the adapter's logs.
	RequestID string `json:"requestId,omitempty"`
	// BackendError is a sanitized message from the backend service, when the
	// failure originated there.
	BackendError string `json:"backendError,omitempty"`
	// Errors lists every field that failed validation, so a client can fix all
	// of them in one round trip rather than one per attempt.
	Errors []FieldError `json:"errors,omitempty"`
}

// FieldError is one field-level validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is a client-facing error bound to a cataloged event. Construct one with
// a taxonomy constructor (NotFound, BackendTimeout, …) rather than directly, so
// the event binding cannot be forgotten.
type Error struct {
	// eventID is the catalog entry that supplies the detail template, response
	// code, and log level.
	eventID events.EventID
	// fields fill the {{key}} placeholders in the event's ResponseDetail and
	// become the event's data group.
	fields map[string]any
	// cause is the internal error. It reaches the operator log and never the
	// client.
	cause error
	// fieldErrors are validation failures rendered into the errors[] extension.
	fieldErrors []FieldError
	// backendError is a sanitized backend message, safe to return.
	backendError string
}

// newError builds an Error for the given catalog event.
func newError(eventID events.EventID, fields map[string]any) *Error {
	return &Error{eventID: eventID, fields: fields}
}

// Error implements error. It renders the operator-facing form — including the
// cause — and is never what a client sees.
func (e *Error) Error() string {
	spec, ok := events.Get(e.eventID)
	if !ok {
		return fmt.Sprintf("unregistered event %s", e.eventID)
	}

	msg := render(spec.ResponseDetail, e.fields)
	if e.cause != nil {
		return msg + ": " + e.cause.Error()
	}

	return msg
}

// Unwrap exposes the internal cause to errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.cause }

// EventID returns the catalog event backing this error.
func (e *Error) EventID() events.EventID { return e.eventID }

// clone returns a copy of the error with its slice independent of the original.
//
// The builders below copy rather than mutate so a shared *Error is safe: a
// package-level sentinel (var errMissing = apierror.NotFound("lease")) decorated
// per request would otherwise be written by every goroutine at once, racing on
// the cause and leaking one request's detail into another's response.
func (e *Error) clone() *Error {
	copied := *e
	copied.fieldErrors = slices.Clone(e.fieldErrors)

	return &copied
}

// WithCause returns a copy carrying the internal error that led here. The cause
// is logged with the event and never appears in the response body.
func (e *Error) WithCause(cause error) *Error {
	next := e.clone()
	next.cause = cause

	return next
}

// WithBackendError returns a copy carrying a sanitized backend message, surfaced
// to the client in the backendError extension. Only pass text that is safe to
// return — callers are responsible for stripping credentials and internal
// hostnames.
func (e *Error) WithBackendError(message string) *Error {
	next := e.clone()
	next.backendError = message

	return next
}

// WithFieldErrors returns a copy carrying additional field-level validation
// failures.
func (e *Error) WithFieldErrors(fieldErrors ...FieldError) *Error {
	next := e.clone()
	next.fieldErrors = append(next.fieldErrors, fieldErrors...)

	return next
}
