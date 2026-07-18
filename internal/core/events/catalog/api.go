package catalog

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// API event IDs. Range 010–019 is reserved for the request lifecycle; 900–999
// for client-facing errors, each backing one problem+json response.
const (
	// API010 is emitted once per HTTP request after the handler returns.
	API010 events.EventID = "API-010"
	// API011 is emitted when a handler panics and recovery returns 500.
	API011 events.EventID = "API-011"

	// API900 backs a 400 validation-failed response.
	API900 events.EventID = "API-900"
	// API901 backs a 401 unauthorized response.
	API901 events.EventID = "API-901"
	// API902 backs a 404 not-found response.
	API902 events.EventID = "API-902"
	// API903 backs a 409 conflict response.
	API903 events.EventID = "API-903"
	// API904 backs a 412 precondition-failed response.
	API904 events.EventID = "API-904"
	// API905 backs a 502 backend-unreachable response.
	API905 events.EventID = "API-905"
	// API906 backs a 502 backend-error response.
	API906 events.EventID = "API-906"
	// API907 backs a 504 backend-timeout response.
	API907 events.EventID = "API-907"
	// API908 backs a 500 internal response — the catch-all for an error that
	// reached the HTTP boundary without a taxonomy entry.
	API908 events.EventID = "API-908"
)

// callerFields are the standard caller fields every ExternalSource event must
// declare; Register panics without them.
var callerFields = []events.FieldDef{
	{Name: "subject", Type: "string", Required: false, Description: "Authenticated caller (empty until auth lands)."},
	{Name: "role", Type: "string", Required: false, Description: "Caller role (empty until auth)."},
	{Name: "remoteAddr", Type: "string", Required: true, Description: "Client address."},
}

// clientError describes one entry in the client-facing error range. Declaring
// them as data keeps the nine registrations honest: every one carries the same
// shape, and a missing ResponseCode is impossible rather than merely caught.
type clientError struct {
	id       events.EventID
	level    slog.Level
	message  string
	detail   string // ResponseDetail: curated, client-visible, {{key}} placeholders
	code     events.ResponseCode
	fields   []events.FieldDef
	describe string
	fix      string
}

// clientErrors is the API-9xx range. 4xx causes are logged at debug (the client
// misbehaved, not the adapter); 5xx at error (the operator must act).
var clientErrors = []clientError{
	{
		id: API900, level: slog.LevelDebug, message: "request rejected: validation failed",
		detail: "The request failed validation.", code: events.CodeValidationFailed,
		fields:   []events.FieldDef{{Name: "fieldErrors", Type: "int", Required: false, Description: "Number of failing fields."}},
		describe: "A request was rejected because it failed validation. The response lists every failing field.",
		fix:      "Client-side fault. Read the errors[] array in the response body; each entry names a field and why it failed.",
	},
	{
		id: API901, level: slog.LevelDebug, message: "request rejected: unauthorized",
		detail: "{{reason}}", code: events.CodeUnauthorized,
		fields:   []events.FieldDef{{Name: "reason", Type: "string", Required: true, Description: "Client-safe reason the credential was rejected."}},
		describe: "A request was rejected because its credential was missing, malformed, or unknown.",
		fix: "Check the caller sends 'Authorization: Bearer <token>' with the full scheme, and that the token is " +
			"listed by `token list` and not expired. See docs/token-management.md.",
	},
	{
		id: API902, level: slog.LevelDebug, message: "request rejected: not found",
		detail: "The requested {{resource}} was not found.", code: events.CodeNotFound,
		fields:   []events.FieldDef{{Name: "resource", Type: "string", Required: true, Description: "The resource that was not found."}},
		describe: "A request addressed a resource that does not exist.",
		fix:      "Usually a stale client cache or a deleted resource. Confirm the identifier against a list call.",
	},
	{
		id: API903, level: slog.LevelDebug, message: "request rejected: conflict",
		detail: "{{reason}}", code: events.CodeConflict,
		fields:   []events.FieldDef{{Name: "reason", Type: "string", Required: true, Description: "Client-safe description of the conflict."}},
		describe: "A request conflicted with the current state of the resource.",
		fix:      "Re-read the resource and retry against its current state.",
	},
	{
		id: API904, level: slog.LevelDebug, message: "request rejected: precondition failed",
		detail: "The resource changed since the version the request expected.", code: events.CodePreconditionFailed,
		fields:   []events.FieldDef{{Name: "expected", Type: "string", Required: false, Description: "The If-Match value supplied."}},
		describe: "A conditional request failed because the resource changed since the client last read it.",
		fix:      "Expected under concurrent writes. The client should re-read, re-apply, and retry with the new ETag.",
	},
	{
		id: API905, level: slog.LevelError, message: "backend unreachable",
		detail: "The {{backend}} backend could not be reached.", code: events.CodeBackendUnreachable,
		fields:   []events.FieldDef{{Name: "backend", Type: "string", Required: true, Description: "The backend that could not be reached."}},
		describe: "The adapter could not contact its backend service at all.",
		fix:      "Check the backend is running and reachable from this host: DNS, routing, firewall, and credentials.",
	},
	{
		id: API906, level: slog.LevelError, message: "backend error",
		detail: "The {{backend}} backend reported an error.", code: events.CodeBackendError,
		fields:   []events.FieldDef{{Name: "backend", Type: "string", Required: true, Description: "The backend that failed."}},
		describe: "The backend was reached but answered with a failure.",
		fix:      "Read the backendError field in the response and the backend's own logs; the fault is on the backend side.",
	},
	{
		id: API907, level: slog.LevelError, message: "backend timeout",
		detail: "The {{backend}} backend did not respond in time.", code: events.CodeBackendTimeout,
		fields:   []events.FieldDef{{Name: "backend", Type: "string", Required: true, Description: "The backend that timed out."}},
		describe: "A backend call exceeded its deadline.",
		fix:      "Check backend load and health. Persistent timeouts mean the backend is overloaded or the timeout is too tight.",
	},
	{
		id: API908, level: slog.LevelError, message: "internal error",
		detail: "An unexpected error occurred.", code: events.CodeInternal,
		fields: []events.FieldDef{{Name: "error", Type: "string", Required: true, Description: "The internal error. Never sent to the client."}},
		describe: "An error reached the HTTP boundary without a taxonomy entry. The client gets a generic 500; " +
			"the cause is recorded here only.",
		fix: "An adapter bug: some path returns an error that is not an apierror. Read the error field, " +
			"then map that failure onto a taxonomy entry at its source.",
	},
}

func init() {
	events.Register(&events.Event{
		ID:              API010,
		Level:           slog.LevelInfo,
		MessageTemplate: "request completed",
		Description:     "Emitted once per HTTP request after the handler returns; the audit line for every request.",
		Category:        events.CategoryAPI.String(),
		Topic:           "Request",
		ExternalSource:  true,
		Fields: []events.FieldDef{
			{Name: "subject", Type: "string", Required: false, Description: "Authenticated caller (empty until auth lands in M2)."},
			{Name: "role", Type: "string", Required: false, Description: "Caller role (empty until auth)."},
			{Name: "remoteAddr", Type: "string", Required: true, Description: "Client address."},
			{Name: "requestId", Type: "string", Required: true, Description: "Correlation ID (X-Request-Id)."},
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Request path."},
			{Name: "status", Type: "int", Required: true, Description: "Response status code."},
			{Name: "durationMs", Type: "int", Required: true, Description: "Handler duration in milliseconds."},
			{Name: "bytesWritten", Type: "int", Required: true, Description: "Response body bytes written."},
		},
		Example:         `{"eventId":"API-010","caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…","method":"GET","path":"/api/v1/health"},"data":{"status":200,"durationMs":1,"bytesWritten":147}}`,
		Troubleshooting: "Informational request audit line. For error spikes, filter status>=500 and correlate related events by requestId.",
	})

	events.Register(&events.Event{
		ID:              API011,
		Level:           slog.LevelError,
		MessageTemplate: "request panic recovered",
		Description:     "A handler panicked; the recovery middleware logged it and returned 500.",
		Category:        events.CategoryAPI.String(),
		Topic:           "Request",
		Fields: []events.FieldDef{
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Request path."},
			{Name: "remoteAddr", Type: "string", Required: true, Description: "Client address."},
			{Name: "requestId", Type: "string", Required: false, Description: "Correlation ID, if the request-ID middleware already ran."},
			{Name: "panic", Type: "string", Required: true, Description: "The recovered panic value."},
			{Name: "stack", Type: "string", Required: false, Description: "Stack trace captured at the panic."},
		},
		Example: `{"eventId":"API-011","data":{"method":"GET","path":"/x","remoteAddr":"192.0.2.1:1234","requestId":"…","panic":"runtime error: invalid memory address"}}`,
		Troubleshooting: "A handler bug caused a panic. Read the stack field, reproduce via method+path, and fix the root cause " +
			"(often a nil dereference or out-of-range index). Correlate other events by requestId.",
	})

	for _, ce := range clientErrors {
		events.Register(&events.Event{
			ID:              ce.id,
			Level:           ce.level,
			MessageTemplate: ce.message,
			Description:     ce.describe,
			Category:        events.CategoryAPI.String(),
			Topic:           "Errors",
			ExternalSource:  true,
			Fields:          append(slices.Clone(callerFields), ce.fields...),
			Example: fmt.Sprintf(`{"eventId":%q,"caller":{"remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"…"},"data":{}}`,
				string(ce.id)),
			Troubleshooting: ce.fix,
			ResponseDetail:  ce.detail,
			ResponseCode:    ce.code,
			Impacts:         []events.Impact{events.ImpactRequestRejected},
		})
	}
}
