package winsvc

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// eventIDAttr is the attribute Emit puts the catalog ID in. The sink reads the
// record rather than being told separately, because slog.Handler is the only
// seam the events system exposes and it hands over a record.
const eventIDAttr = "eventId"

// mirroredEvent returns the catalog entry behind a record, and whether the
// record belongs in the Event Log at all.
//
// The decision is the registry's, not this function's: an event carries its own
// EventLogID, declared by the package that owns it, and a zero means "not
// mirrored". Reading it back here rather than keeping a table means the sink
// cannot drift from the catalog, and a conformance test in internal/catalogs
// holds the catalog to the eligibility rule.
//
// The whole entry is returned rather than just the number, because rendering
// needs it too: a field the catalog marks EventLogOmit is left out of the
// entry, and the catalog is the only place that says which.
//
// A record carrying no eventId is a raw slog call. Those are developer
// breadcrumbs by convention, so they stay out of an operator's Event Viewer.
func mirroredEvent(r slog.Record) (*events.Event, bool) {
	var id events.EventID

	r.Attrs(func(a slog.Attr) bool {
		if a.Key != eventIDAttr {
			return true
		}

		id = events.EventID(a.Value.String())

		return false
	})

	if id == "" {
		return nil, false
	}

	event, known := events.Get(id)
	if !known || event.EventLogID == 0 {
		return nil, false
	}

	return event, true
}

// omittedFields is the set of field names an event keeps out of its Event Log
// entry.
//
// Matched on the LEAF name, which is the name a FieldDef carries: Emit nests
// an event's data under a "data" group, so the attribute this sink walks is
// "stack" inside that group rather than "data.stack". A leaf match also covers
// the same field wherever else it were to appear on the record, which for a
// field declared too sensitive for this log is the answer that errs the right
// way.
func omittedFields(event *events.Event) []string {
	var out []string

	for _, f := range event.Fields {
		if f.EventLogOmit {
			out = append(out, f.Name)
		}
	}

	return out
}

// eventLogWriter is what the platform half implements: one entry, at a level.
type eventLogWriter interface {
	info(eid uint32, msg string) error
	warning(eid uint32, msg string) error
	error(eid uint32, msg string) error
	Close() error
}

// eventLogHandler mirrors cataloged events into the Windows Event Log.
//
// It is deliberately not a second copy of the log. The file carries the whole
// structured stream; this carries the handful of lines an operator opens Event
// Viewer for, because a service has no console and a file sink cannot promise
// anything about a failure that happened before the file was opened.
type eventLogHandler struct {
	w     eventLogWriter
	attrs []slog.Attr
}

// Enabled accepts INFO and above and lets Handle decide the rest.
//
// The real filter is per-event rather than per-level: eligibility is a property
// of the catalog entry, and SYS-001 at INFO belongs here while an INFO request
// log does not. But the floor still matters, because the fan-out ORs its
// children -- a handler that answered true at every level would have slog build
// a record for every slog.Debug breadcrumb in the process, whatever
// logSeverity said, purely so this handler could drop it.
//
// INFO is safe rather than arbitrary: no DEBUG event can carry an EventLogID,
// since the conformance test in internal/catalogs rejects an ID on anything the
// eligibility rule excludes, and that rule takes WARN and above plus two INFO
// anchors.
func (h *eventLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// Handle writes one entry, or drops the record when it is not mirrored.
func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	event, mirrored := mirroredEvent(r)
	if !mirrored {
		return nil
	}

	msg := h.render(r, omittedFields(event))

	switch {
	case r.Level >= slog.LevelError:
		return h.w.error(event.EventLogID, msg)
	case r.Level >= slog.LevelWarn:
		return h.w.warning(event.EventLogID, msg)
	default:
		return h.w.info(event.EventLogID, msg)
	}
}

// WithAttrs returns a handler carrying attrs on every entry.
func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	next = append(next, h.attrs...)
	next = append(next, attrs...)

	return &eventLogHandler{w: h.w, attrs: next}
}

// WithGroup returns the handler unchanged.
//
// The Event Log entry is a flat string, so there is no nesting to express.
// Honouring groups by prefixing keys would change the text of an entry whose
// shape an operator reads directly, for no gain.
func (h *eventLogHandler) WithGroup(string) slog.Handler { return h }

// render flattens a record into the single string an Event Log entry carries.
//
// The message first, then every attribute as key=value, because that is the
// order an operator reads: Event Viewer shows the opening words in its list
// column, and the detail only once an entry is selected.
func (h *eventLogHandler) render(r slog.Record, omit []string) string {
	var b strings.Builder

	b.WriteString(r.Message)

	for _, a := range h.attrs {
		appendAttr(&b, "", a, omit)
	}

	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, "", a, omit)

		return true
	})

	return b.String()
}

// appendAttr writes one attribute, flattening a group into dotted keys and
// dropping anything the event keeps out of this log.
func appendAttr(b *strings.Builder, prefix string, a slog.Attr, omit []string) {
	if slices.Contains(omit, a.Key) {
		return
	}

	a.Value = a.Value.Resolve()

	key := a.Key
	if prefix != "" {
		key = prefix + "." + a.Key
	}

	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			// An empty group contributes nothing, and slog's own handlers
			// elide it. Writing "data=" would be noise in a line an operator
			// reads unaided.
			return
		}

		for _, nested := range group {
			appendAttr(b, key, nested, omit)
		}

		return
	}

	b.WriteString(" ")
	b.WriteString(key)
	b.WriteString("=")
	b.WriteString(a.Value.String())
}
