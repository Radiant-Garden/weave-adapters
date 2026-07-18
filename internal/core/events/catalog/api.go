package catalog

import (
	"log/slog"
	"slices"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// API event IDs. Range 010–019 is reserved for the request lifecycle; 900–999
// for client-facing errors, each backing one problem+json response.
//
// Only errors with a live emitter are registered. The rest of the taxonomy in
// 02-shared-core.md arrives with the code that returns it: unauthorized with
// the auth middleware, the backend codes with the first backend client. See
// .claude/guidelines/event-logging.md on ghost events.
const (
	// API010 is emitted once per HTTP request after the handler returns.
	API010 events.EventID = "API-010"
	// API011 is emitted when a handler panics and recovery returns 500.
	API011 events.EventID = "API-011"

	// API900 backs a 404 not-found response.
	API900 events.EventID = "API-900"
	// API901 backs a 500 internal response — the catch-all for an error that
	// reached the HTTP boundary without a taxonomy entry.
	API901 events.EventID = "API-901"
)

// callerFields are the standard caller fields every ExternalSource event must
// declare; Register panics without them.
var callerFields = []events.FieldDef{
	{Name: "subject", Type: "string", Required: false, Description: "Authenticated caller (empty until auth lands)."},
	{Name: "role", Type: "string", Required: false, Description: "Caller role (empty until auth)."},
	{Name: "remoteAddr", Type: "string", Required: true, Description: "Client address."},
}

// clientError describes one entry in the client-facing error range. Declaring
// them as data keeps the registrations honest: every one carries the same
// shape, and a missing ResponseCode is impossible rather than merely caught.
type clientError struct {
	id       events.EventID
	level    slog.Level
	message  string
	detail   string // ResponseDetail: curated, client-visible, {{key}} placeholders
	code     events.ResponseCode
	fields   []events.FieldDef
	example  string
	describe string
	fix      string
}

// clientErrors is the API-9xx range. A 4xx cause is logged at debug (the client
// misbehaved, not the adapter); a 5xx at error (the operator must act).
var clientErrors = []clientError{
	{
		id: API900, level: slog.LevelDebug, message: "request rejected: not found",
		detail: "The requested {{resource}} was not found.", code: events.CodeNotFound,
		fields:   []events.FieldDef{{Name: "resource", Type: "string", Required: true, Description: "The resource that was not found."}},
		example:  `{"eventId":"API-900","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/openapi.yaml"},"data":{"resource":"openapi document"}}`,
		describe: "A request addressed a resource that does not exist.",
		fix:      "Usually a stale client cache or a deleted resource. Confirm the identifier against a list call.",
	},
	{
		id: API901, level: slog.LevelError, message: "internal error",
		detail: "An unexpected error occurred.", code: events.CodeInternal,
		fields:  []events.FieldDef{{Name: "error", Type: "string", Required: true, Description: "The internal error. Never sent to the client."}},
		example: `{"eventId":"API-901","caller":{"subject":"","role":"","remoteAddr":"192.0.2.1:1234"},"request":{"requestId":"9f1c…","method":"GET","path":"/api/v1/leases"},"data":{"error":"dial tcp 10.0.0.9:445: connect: connection refused"}}`,
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
		Fields: append(slices.Clone(callerFields), []events.FieldDef{
			{Name: "requestId", Type: "string", Required: true, Description: "Correlation ID (X-Request-Id)."},
			{Name: "method", Type: "string", Required: true, Description: "HTTP method."},
			{Name: "path", Type: "string", Required: true, Description: "Request path."},
			{Name: "status", Type: "int", Required: true, Description: "Response status code."},
			{Name: "durationMs", Type: "int", Required: true, Description: "Handler duration in milliseconds."},
			{Name: "bytesWritten", Type: "int", Required: true, Description: "Response body bytes written."},
		}...),
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
			Example:         ce.example,
			Troubleshooting: ce.fix,
			ResponseDetail:  ce.detail,
			ResponseCode:    ce.code,
			Impacts:         []events.Impact{events.ImpactRequestRejected},
		})
	}
}
