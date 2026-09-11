package observability

import (
	"context"
	"errors"
	"log/slog"
)

// fanout is a slog.Handler that writes every record to several handlers.
//
// It exists because the service path needs two sinks at once and they answer
// different questions: the file carries the complete structured stream for
// whoever is reading logs, and the Windows Event Log carries the handful of
// lines an operator opens Event Viewer for when a start failed. Neither
// replaces the other, and the alternative -- installing one handler and having
// the second reach around it -- means whichever runs second silently wins.
type fanout struct {
	handlers []slog.Handler
}

// newFanout returns a handler writing to all of them, or the single handler
// unwrapped when there is only one. Unwrapping is not an optimisation: it keeps
// the common console path exactly the handler it was before this type existed.
func newFanout(handlers ...slog.Handler) slog.Handler {
	if len(handlers) == 1 {
		return handlers[0]
	}

	return &fanout{handlers: handlers}
}

// Enabled reports whether any child would handle the level.
//
// The OR is load-bearing. Gating on the first handler -- the file, whose level
// comes from logSeverity -- would silence the Event Log arm entirely the moment
// an operator set logSeverity=error, which is exactly the setting someone
// reaches for when they want *less* noise in a file and no less of it in Event
// Viewer.
func (f *fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}

	return false
}

// Handle passes the record to every child that wants it.
//
// Each gets its own clone: slog.Handler implementations are permitted to retain
// or mutate the record they are given, so handing the same one to several is
// the kind of aliasing bug that shows up as a corrupted log line under load and
// nowhere else.
func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error

	for _, h := range f.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}

		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}

	// Joined rather than first-wins: a failing Event Log sink must not hide a
	// failing file sink, and slog discards this either way -- it is here so a
	// test can see both.
	return errors.Join(errs...)
}

// WithAttrs returns a fanout whose children all carry the attributes.
func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}

	return &fanout{handlers: next}
}

// WithGroup returns a fanout whose children are all grouped.
func (f *fanout) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}

	return &fanout{handlers: next}
}
