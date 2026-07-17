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
		Fields: []events.FieldDef{
			{Name: "signal", Type: "string", Required: false, Description: "The signal that triggered shutdown."},
		},
		Example:         `{"eventId":"SYS-003","data":{"signal":"terminated"}}`,
		Troubleshooting: "Informational. Follows SIGINT/SIGTERM or a cancelled run context.",
	})

	events.Register(&events.Event{
		ID:              SYS004,
		Level:           slog.LevelInfo,
		MessageTemplate: "shutdown complete",
		Description:     "The server drained and shut down cleanly.",
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
}
