package catalog

import (
	"log/slog"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// API event IDs. Range 010–019 is reserved for the request lifecycle.
const (
	// API010 is emitted once per HTTP request after the handler returns.
	API010 events.EventID = "API-010"
	// API011 is emitted when a handler panics and recovery returns 500.
	API011 events.EventID = "API-011"
)

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
}
