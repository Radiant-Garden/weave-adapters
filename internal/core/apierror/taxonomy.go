package apierror

import (
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// typePrefix namespaces problem types so a client can tell an adapter error
// from anything else it might be handed.
const typePrefix = "weave-adapters:"

// entry is the HTTP meaning of one response code.
type entry struct {
	status int
	title  string
}

// taxonomy maps every response code to its HTTP status and title. This is the
// whole client-facing error vocabulary: adapters map their backend failures onto
// these codes and may add their own, but never invent a second error shape.
//
// Codes are added by the milestone that needs them — 405, 413 and 428 arrive
// with the middleware and ETag write side that produce them.
var taxonomy = map[events.ResponseCode]entry{
	events.CodeValidationFailed:   {status: http.StatusBadRequest, title: "Validation failed"},
	events.CodeUnauthorized:       {status: http.StatusUnauthorized, title: "Unauthorized"},
	events.CodeNotFound:           {status: http.StatusNotFound, title: "Not found"},
	events.CodeConflict:           {status: http.StatusConflict, title: "Conflict"},
	events.CodePreconditionFailed: {status: http.StatusPreconditionFailed, title: "Precondition failed"},
	events.CodeBackendUnreachable: {status: http.StatusBadGateway, title: "Backend unreachable"},
	events.CodeBackendError:       {status: http.StatusBadGateway, title: "Backend error"},
	events.CodeBackendTimeout:     {status: http.StatusGatewayTimeout, title: "Backend timeout"},
	events.CodeInternal:           {status: http.StatusInternalServerError, title: "Internal server error"},
}

// lookup returns the HTTP meaning of a response code. An unknown code resolves
// to a 500 rather than a zero status: a catalog entry that names a code the
// taxonomy does not know is a bug, and answering 0 would be worse than
// answering 500. TestTaxonomy_ShouldCoverEveryRegisteredResponseCode makes the
// bug impossible to ship.
func lookup(code events.ResponseCode) entry {
	if e, ok := taxonomy[code]; ok {
		return e
	}

	return taxonomy[events.CodeInternal]
}

// TypeFor returns the problem+json type slug for a response code. Exported for
// the few callers that build a Problem directly instead of going through an
// *Error — the recovery middleware, which already emitted its own event.
func TypeFor(code events.ResponseCode) string {
	return typePrefix + string(code)
}

// TitleFor returns the problem+json title for a response code.
func TitleFor(code events.ResponseCode) string {
	return lookup(code).title
}

// ValidationFailed reports a request that failed validation. Pass every failing
// field: clients fix them in one round trip rather than one per attempt.
func ValidationFailed(fieldErrors ...FieldError) *Error {
	return newError(catalog.API900, map[string]any{
		"fieldErrors": len(fieldErrors),
	}).WithFieldErrors(fieldErrors...)
}

// Unauthorized reports a missing, malformed, or unknown credential. reason is
// returned to the client, so it must describe what is expected without
// confirming what was wrong with the credential presented.
func Unauthorized(reason string) *Error {
	return newError(catalog.API901, map[string]any{"reason": reason})
}

// NotFound reports an addressed resource that does not exist. resource is
// echoed to the client, so name the kind and identifier, not internal state.
func NotFound(resource string) *Error {
	return newError(catalog.API902, map[string]any{"resource": resource})
}

// Conflict reports a request that conflicts with current state.
func Conflict(reason string) *Error {
	return newError(catalog.API903, map[string]any{"reason": reason})
}

// PreconditionFailed reports a conditional request whose precondition did not
// hold — the resource changed since the client last read it.
func PreconditionFailed(expected string) *Error {
	return newError(catalog.API904, map[string]any{"expected": expected})
}

// BackendUnreachable reports a backend that could not be contacted at all.
func BackendUnreachable(backend string) *Error {
	return newError(catalog.API905, map[string]any{"backend": backend})
}

// BackendError reports a backend that answered with a failure.
func BackendError(backend string) *Error {
	return newError(catalog.API906, map[string]any{"backend": backend})
}

// BackendTimeout reports a backend call that exceeded its deadline.
func BackendTimeout(backend string) *Error {
	return newError(catalog.API907, map[string]any{"backend": backend})
}

// Internal reports an unexpected fault. The cause is recorded for the operator
// and never returned to the client — this is the redaction boundary, and the
// reason an unmapped error becomes a generic 500 rather than leaking a message
// that may name internal hosts, queries, or paths.
func Internal(cause error) *Error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	return newError(catalog.API908, map[string]any{"error": message}).WithCause(cause)
}
