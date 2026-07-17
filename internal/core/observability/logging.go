// Package observability wires the adapter's logging sink. Code does not call
// slog directly for anything noteworthy — it emits cataloged events (see
// internal/core/events), which write through the slog logger configured here.
//
// Metrics (Prometheus) are intentionally not part of M1 and are planned for a
// later milestone; this package is logging-only for now.
package observability

import (
	"log/slog"
	"os"
)

// levelFor maps a validated severity string to an slog.Level. Unknown values
// fall back to info (config validation rejects them before we get here).
func levelFor(severity string) slog.Level {
	switch severity {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger builds the process logger at the given severity. It uses a text
// handler on stdout for now; a JSON handler and a format switch arrive with the
// production logging config in a later milestone.
func NewLogger(severity string) *slog.Logger {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: levelFor(severity),
	})

	return slog.New(handler)
}

// Setup builds the logger and installs it as the slog default, so the events
// system (which logs through slog.Default) writes through it. It returns the
// logger for callers that want to hold a reference.
func Setup(severity string) *slog.Logger {
	logger := NewLogger(severity)
	slog.SetDefault(logger)

	return logger
}
