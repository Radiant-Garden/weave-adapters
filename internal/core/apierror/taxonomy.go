package apierror

import (
	"fmt"
	"net/http"
	"strings"

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
// vocabulary in 02-shared-core.md arrives with its emitter: conflict/precondition
// (409/412), and 428 with the ETag write side that produces it.
//
// The backend codes are 502/504 rather than 500 because they say the fault was
// downstream. An adapter is a gateway: a 500 claims the adapter itself is
// broken, which sends an operator to the wrong logs, and weave — which reads
// the status code and never the body — cannot tell the two apart otherwise.
var taxonomy = map[events.ResponseCode]entry{
	events.CodeValidationFailed: {status: http.StatusBadRequest, title: "Validation failed"},
	events.CodeUnauthorized:     {status: http.StatusUnauthorized, title: "Unauthorized"},
	events.CodeNotFound:         {status: http.StatusNotFound, title: "Not found"},
	events.CodeMethodNotAllowed: {status: http.StatusMethodNotAllowed, title: "Method not allowed"},
	events.CodeInternal:         {status: http.StatusInternalServerError, title: "Internal server error"},

	events.CodePayloadTooLarge:      {status: http.StatusRequestEntityTooLarge, title: "Payload too large"},
	events.CodeUnsupportedMediaType: {status: http.StatusUnsupportedMediaType, title: "Unsupported media type"},

	events.CodeBackendUnavailable: {status: http.StatusBadGateway, title: "Backend unavailable"},
	events.CodeBackendTimeout:     {status: http.StatusGatewayTimeout, title: "Backend timeout"},
	events.CodeBackendError:       {status: http.StatusBadGateway, title: "Backend error"},
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
//
// It panics when the event declares no response, because the alternative is
// serving a problem+json whose type is a bare "weave-adapters:" and whose detail
// is empty — a malformed body that looks like a backend quirk to whoever
// receives it. The id is always a catalog constant, so this is a wiring mistake,
// and the first request down the path is where it should surface.
func New(id events.EventID, fields ...any) *Error {
	spec, ok := events.Get(id)
	if !ok || spec.ResponseCode == "" || spec.ResponseDetail == "" {
		panic(fmt.Sprintf("apierror: event %s declares no ResponseCode/ResponseDetail, so it cannot be an API error", id))
	}

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

// Validation reports rejected request parameters or body fields. Pass every
// failure at once: the client fixes them all in one round trip rather than
// discovering them one attempt at a time, which is why the errors[] extension
// exists.
//
// Both the field name and its message reach the client, so a message must
// describe the expectation ("must be at least 1") and never internal state.
func Validation(fieldErrors ...FieldError) *Error {
	fields := make([]string, 0, len(fieldErrors))
	for _, fe := range fieldErrors {
		fields = append(fields, fe.Field)
	}

	// The catalog declares fields as required, and a caller that passed nothing
	// would otherwise log an empty one. "(none)" matches the placeholder API-021
	// already uses for an absent scheme, and keeps a malformed diagnostic from
	// taking down the request it was describing — the same call this package
	// makes in pairsToMap.
	named := "(none)"
	if len(fields) > 0 {
		named = strings.Join(fields, ", ")
	}

	// The event carries the field names only. Their messages are already in the
	// response and are generic by construction, so repeating them in the log
	// would add length without adding a diagnostic.
	return newError(catalog.API903, map[string]any{"fields": named}).WithFieldErrors(fieldErrors...)
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
