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

// taxonomy maps every response code to its HTTP status and title. Adapters map
// their backend failures onto these codes and may add their own, but never
// invent a second error shape.
//
// Entries exist only for codes something actually returns. The rest of the
// vocabulary in 02-shared-core.md arrives with its emitter:
// conflict/precondition (409/412) and the backend codes (502/504) with the
// first backend client, 405/413/428 with the middleware and ETag write side
// that produce them.
var taxonomy = map[events.ResponseCode]entry{
	events.CodeUnauthorized:     {status: http.StatusUnauthorized, title: "Unauthorized"},
	events.CodeNotFound:         {status: http.StatusNotFound, title: "Not found"},
	events.CodeMethodNotAllowed: {status: http.StatusMethodNotAllowed, title: "Method not allowed"},
	events.CodeInternal:         {status: http.StatusInternalServerError, title: "Internal server error"},
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

// New builds an error bound to any cataloged event, for packages that own their
// own error events rather than using the constructors below — the auth
// middleware owns API-02x, and adapters own their categories. The event must
// declare a ResponseDetail and ResponseCode; fields are key/value pairs filling
// its {{key}} placeholders.
//
// This is the seam that keeps one rejection to one event: the owning package
// binds its own diagnostic rather than emitting its event and then asking a core
// constructor to emit a second one.
func New(id events.EventID, fields ...any) *Error {
	return newError(id, pairsToMap(fields))
}

// pairsToMap turns Emit-style key/value pairs into a field map. An odd trailing
// key is dropped rather than panicking: a malformed diagnostic must not take
// down the request it was describing.
func pairsToMap(pairs []any) map[string]any {
	fields := make(map[string]any, len(pairs)/2)

	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}

		fields[key] = pairs[i+1]
	}

	return fields
}

// NotFound reports an addressed resource that does not exist. resource is
// echoed to the client, so name the kind and identifier, not internal state.
func NotFound(resource string) *Error {
	return newError(catalog.API900, map[string]any{"resource": resource})
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

	return newError(catalog.API901, map[string]any{"error": message}).WithCause(cause)
}
