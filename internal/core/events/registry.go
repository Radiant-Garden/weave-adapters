package events

import (
	"fmt"
	"maps"
	"strings"
	"sync"
)

// registry holds all registered events.
type registry struct {
	mu     sync.RWMutex
	events map[EventID]*Event
}

var globalRegistry = &registry{
	events: make(map[EventID]*Event),
}

// standardCallerFields are the fields an ExternalSource event must declare, so
// the catalog documents the caller identity that Emit attaches at runtime.
var standardCallerFields = []string{"subject", "role", "remoteAddr"}

// Register adds an event to the global registry. It is called from init() in
// the catalog packages and panics on contract violations so they surface at
// process start, not in production:
//   - duplicate ID
//   - ResponseDetail without ResponseCode
//   - ExternalSource event whose Fields omit a standard caller field
func Register(event *Event) {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()

	if _, exists := globalRegistry.events[event.ID]; exists {
		panic(fmt.Sprintf("event %s already registered", event.ID))
	}

	if event.ResponseDetail != "" && event.ResponseCode == "" {
		panic(fmt.Sprintf("event %s has ResponseDetail but no ResponseCode", event.ID))
	}

	if event.ExternalSource {
		has := make(map[string]bool, len(event.Fields))
		for _, f := range event.Fields {
			has[f.Name] = true
		}

		for _, required := range standardCallerFields {
			if !has[required] {
				panic(fmt.Sprintf(
					"event %s is ExternalSource but Fields is missing %q; add the standard caller field",
					event.ID, required,
				))
			}
		}
	}

	globalRegistry.events[event.ID] = event
}

// Get retrieves a registered event by ID.
func Get(id EventID) (*Event, bool) {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	event, ok := globalRegistry.events[id]

	return event, ok
}

// GetAll returns a copy of every registered event keyed by ID.
func GetAll() map[EventID]*Event {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	out := make(map[EventID]*Event, len(globalRegistry.events))
	maps.Copy(out, globalRegistry.events)

	return out
}

// getByCategory returns all events whose ID begins with the category prefix.
func getByCategory(category EventCategory) []*Event {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	prefix := string(category) + "-"

	var result []*Event

	for id, event := range globalRegistry.events {
		if strings.HasPrefix(string(id), prefix) {
			result = append(result, event)
		}
	}

	return result
}

// count returns the number of registered events.
func count() int {
	globalRegistry.mu.RLock()
	defer globalRegistry.mu.RUnlock()

	return len(globalRegistry.events)
}
