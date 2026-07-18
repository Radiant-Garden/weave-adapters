// Package catalog declares and registers the core (adapter-agnostic) event
// catalog. Each file registers one category from init(); the registry panics on
// contract violations, so any mistake surfaces at process start.
package catalog

import (
	"log/slog"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// SYS event IDs (system lifecycle). Range 001–009 is reserved for status.
const (
	// SYS001 is emitted when the adapter process starts.
	SYS001 events.EventID = "SYS-001"
	// SYS002 is emitted when the HTTP server is listening and ready.
	SYS002 events.EventID = "SYS-002"
	// SYS003 is emitted when a shutdown signal is received.
	SYS003 events.EventID = "SYS-003"
	// SYS004 is emitted when shutdown has completed cleanly.
	SYS004 events.EventID = "SYS-004"
	// SYS005 is emitted when the process fails to start.
	SYS005 events.EventID = "SYS-005"
	// SYS006 is emitted at startup when authentication is disabled.
	SYS006 events.EventID = "SYS-006"
	// SYS007 is emitted when the drain grace period expired before shutdown
	// finished.
	SYS007 events.EventID = "SYS-007"
)

func init() {
	events.Register(&events.Event{
		ID:              SYS001,
		Level:           slog.LevelInfo,
		MessageTemplate: "adapter starting",
		Description:     "The adapter process has started and is initializing.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Fields: []events.FieldDef{
			{Name: "version", Type: "string", Required: true, Description: "Adapter build version."},
		},
		Example:         `{"eventId":"SYS-001","data":{"version":"1.2.3"}}`,
		Troubleshooting: "Informational. Marks the beginning of a process lifecycle.",
	})

	events.Register(&events.Event{
		ID:              SYS002,
		Level:           slog.LevelInfo,
		MessageTemplate: "listening",
		Description:     "The HTTP server is listening and ready to serve requests.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Fields: []events.FieldDef{
			{Name: "addr", Type: "string", Required: true, Description: "Listen address (host:port)."},
		},
		Example:         `{"eventId":"SYS-002","data":{"addr":":8444"}}`,
		Troubleshooting: "If this never appears, the server failed to bind; check the port and permissions.",
	})

	events.Register(&events.Event{
		ID:              SYS003,
		Level:           slog.LevelInfo,
		MessageTemplate: "shutdown initiated",
		Description:     "A termination signal was received; the server is draining.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		// No "signal" field: signal.NotifyContext absorbs the signal's identity
		// into a plain context, so by the time httpserver sees ctx.Done() there
		// is nothing left to name. Documenting a field nothing can populate is
		// the no-ghost-events rule one level down.
		Example:         `{"eventId":"SYS-003","data":{}}`,
		Troubleshooting: "Informational. Follows SIGINT/SIGTERM or a cancelled run context.",
	})

	events.Register(&events.Event{
		ID:              SYS004,
		Level:           slog.LevelInfo,
		MessageTemplate: "shutdown complete",
		Description:     "The server drained and shut down cleanly. SYS-007 is emitted instead when it did not.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Example:         `{"eventId":"SYS-004","data":{}}`,
		Troubleshooting: "Informational. Marks a clean end to a process lifecycle.",
	})

	events.Register(&events.Event{
		ID:              SYS005,
		Level:           slog.LevelError,
		MessageTemplate: "startup failed",
		Description:     "The process failed to start and is exiting non-zero.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Fields: []events.FieldDef{
			{Name: "error", Type: "string", Required: true, Description: "The startup error."},
		},
		Example: `{"eventId":"SYS-005","data":{"error":"loading config: port must be between 1 and 65535, got 0"}}`,
		Troubleshooting: "The process did not start. Read the error field. Most often it is a " +
			"config problem: check the config file, WEAVE_ADAPTER_* env vars, and flags. " +
			"Validate the port range and logSeverity value, then re-run.",
	})

	events.Register(&events.Event{
		ID:              SYS006,
		Level:           slog.LevelWarn,
		MessageTemplate: "authentication disabled",
		Description: "The adapter started with disableAuth set: every route except health is open to anyone " +
			"who can reach the port.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Example:         `{"eventId":"SYS-006","data":{}}`,
		Troubleshooting: "Development-only setting. Unset disableAuth and configure a token (`token gen --label <name>`) before this host is reachable by anything but you.",
	})
	events.Register(&events.Event{
		ID:              SYS007,
		Level:           slog.LevelError,
		MessageTemplate: "shutdown incomplete",
		Description:     "The drain grace period expired with requests still in flight; they were cut off.",
		Category:        events.CategorySystem.String(),
		Topic:           "Lifecycle",
		Fields: []events.FieldDef{
			{Name: "error", Type: "string", Required: true, Description: "The shutdown error."},
			{Name: "graceSeconds", Type: "int", Required: true, Description: "The drain grace period that expired."},
		},
		Example: `{"eventId":"SYS-007","data":{"error":"context deadline exceeded","graceSeconds":15}}`,
		Troubleshooting: "Clients of the cut-off requests saw a dropped connection and may retry a " +
			"non-idempotent call. Check for a handler that outlives the grace period — a slow backend " +
			"call with no timeout of its own is the usual cause. If the drain is legitimately long, " +
			"raise the grace period; otherwise bound the handler.",
	})
}
