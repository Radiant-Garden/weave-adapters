// Package observability wires the adapter's logging sink. Code does not call
// slog directly for anything noteworthy — it emits cataloged events (see
// internal/core/events), which write through the slog logger configured here.
//
// Metrics (Prometheus) are intentionally not part of M1 and are planned for a
// later milestone; this package is logging-only for now.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// logFileMode keeps the log owner-only. The stream carries event data rather
// than credentials, so this is hygiene rather than the security boundary --
// Windows largely ignores Unix permission bits, and the real protection there
// is the directory ACL the installer applies.
const logFileMode = 0o600

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

// newHandler builds the primary handler at the given severity, writing to w.
// It is a text handler; a JSON handler and a format switch arrive with the
// production logging config in a later milestone.
func newHandler(w io.Writer, severity string) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: levelFor(severity),
	})
}

// nopCloser is the Closer returned when Setup opened nothing. Closing stdout
// would be worse than doing nothing, so the zero case has to be explicit
// rather than handing back the sink itself.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// Setup builds the process logger and installs it as the slog default, so the
// events system (which logs through slog.Default) writes through it.
//
// logFile is where the primary stream goes; empty keeps today's behaviour of
// writing to stdout. It exists because a process started by the Windows
// Service Control Manager has no console and the SCM discards stdout, so the
// console default would silently drop every line the adapter ever logs.
//
// sinks are additional handlers to write to alongside the primary one -- the
// Windows Event Log arm in practice. They are passed in rather than composed
// onto whatever handler happens to be installed: in console mode the installed
// default is slog's own stderr handler, so composing would write every record
// to stdout and stderr both.
//
// The returned Closer owns the log file and nothing else. Callers close it on
// shutdown; the file is opened for append, and os.File writes are unbuffered,
// so this is handle hygiene rather than flush-on-exit.
func Setup(severity, logFile string, sinks ...slog.Handler) (*slog.Logger, io.Closer, error) {
	var (
		out    io.Writer = os.Stdout
		closer io.Closer = nopCloser{}
	)

	if logFile != "" {
		// Append, not truncate: a restart must not erase the log of the run
		// that prompted it, which is the one an operator is about to read.
		//nolint:gosec // G304: operator-supplied path, by design — same as the token store's.
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
		if err != nil {
			return nil, nil, fmt.Errorf("opening log file %q: %w", logFile, err)
		}

		out, closer = f, f
	}

	handlers := append([]slog.Handler{newHandler(out, severity)}, sinks...)

	logger := slog.New(newFanout(handlers...))
	slog.SetDefault(logger)

	return logger, closer, nil
}
