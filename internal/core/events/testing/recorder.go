// Package testing provides a recorder and assertions for testing event
// emission. Adapter and core tests assert "endpoint X emitted API-030 with
// these fields" instead of scraping log output. Import it with an alias, e.g.
// eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing".
package testing

import (
	"log/slog"
	"sync"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// RecordedEvent is one captured emission, with its slog groups decoded.
type RecordedEvent struct {
	ID     events.EventID
	Groups map[string]map[string]any // group name -> {field: value}
	Spec   *events.Event             // catalog spec, nil if the ID was unregistered
}

func (e RecordedEvent) group(name, key string) any {
	if m, ok := e.Groups[name]; ok {
		return m[key]
	}

	return nil
}

// has reports whether the named group carries the given key, regardless of the
// value (so a legitimately zero/nil field still counts as present).
func (e RecordedEvent) has(name, key string) bool {
	m, ok := e.Groups[name]
	if !ok {
		return false
	}

	_, ok = m[key]

	return ok
}

// Data returns a value from the event's "data" group (nil if absent).
func (e RecordedEvent) Data(key string) any { return e.group("data", key) }

// Caller returns a value from the event's "caller" group (nil if absent).
func (e RecordedEvent) Caller(key string) any { return e.group("caller", key) }

// Request returns a value from the event's "request" group (nil if absent).
func (e RecordedEvent) Request(key string) any { return e.group("request", key) }

// Recorder captures events emitted during a test.
type Recorder struct {
	mu     sync.Mutex
	events []RecordedEvent
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Install starts capturing emitted events and returns a cleanup function that
// restores the previous hook. Call cleanup with t.Cleanup.
func (r *Recorder) Install() (cleanup func()) {
	prev := events.SetEmitterHook(r.capture)

	return func() { events.SetEmitterHook(prev) }
}

// capture decodes the slog group attributes of one emission and stores it.
func (r *Recorder) capture(id events.EventID, attrs ...any) {
	groups := make(map[string]map[string]any)

	for _, a := range attrs {
		attr, ok := a.(slog.Attr)
		if !ok || attr.Value.Kind() != slog.KindGroup {
			continue
		}

		fields := make(map[string]any)
		for _, sub := range attr.Value.Group() {
			fields[sub.Key] = sub.Value.Any()
		}

		groups[attr.Key] = fields
	}

	spec, _ := events.Get(id)

	r.mu.Lock()
	r.events = append(r.events, RecordedEvent{ID: id, Groups: groups, Spec: spec})
	r.mu.Unlock()
}

// All returns a copy of every recorded event, in emission order.
func (r *Recorder) All() []RecordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]RecordedEvent, len(r.events))
	copy(out, r.events)

	return out
}

// FindByID returns every recorded event with the given ID.
func (r *Recorder) FindByID(id events.EventID) []RecordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []RecordedEvent

	for _, e := range r.events {
		if e.ID == id {
			out = append(out, e)
		}
	}

	return out
}
