/*
Testing: registry.go

Pending:

Tested:
  Register
    - TestRegister_ShouldPanicOnDuplicateID: duplicate IDs panic at registration.
    - TestRegister_ShouldPanicWhenResponseDetailWithoutCode: response-detail invariant.
    - TestRegister_ShouldPanicWhenExternalSourceMissingCallerField: caller-field invariant.
    - TestRegister_ShouldStoreValidEvent: a valid event is retrievable.
  Get / GetAll / getByCategory / count
    - TestRegistry_QueriesReflectRegisteredEvents: lookups and counts.

Tested elsewhere:

Declined:

Additional Remarks:
  These tests mutate the process-global registry, so they cannot run in parallel;
  withCleanRegistry snapshots and restores it around each test.
*/

package events

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCleanRegistry replaces the global registry with an empty one for the
// duration of the test and restores it afterwards.
func withCleanRegistry(t *testing.T) {
	t.Helper()

	globalRegistry.mu.Lock()
	saved := globalRegistry.events
	globalRegistry.events = make(map[EventID]*Event)
	globalRegistry.mu.Unlock()

	t.Cleanup(func() {
		globalRegistry.mu.Lock()
		globalRegistry.events = saved
		globalRegistry.mu.Unlock()
	})
}

// callerFields returns FieldDefs covering the standard caller fields, for
// building valid ExternalSource test events.
func callerFields() []FieldDef {
	fields := make([]FieldDef, 0, len(standardCallerFields))
	for _, n := range standardCallerFields {
		fields = append(fields, FieldDef{Name: n, Type: "string"})
	}

	return fields
}

func TestRegister_ShouldPanicOnDuplicateID(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)

	Register(&Event{ID: "TEST-001", Level: slog.LevelInfo})

	require.Panics(t, func() {
		Register(&Event{ID: "TEST-001", Level: slog.LevelInfo})
	})
}

func TestRegister_ShouldPanicWhenResponseDetailWithoutCode(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)

	require.Panics(t, func() {
		Register(&Event{ID: "TEST-002", ResponseDetail: "something failed"})
	})
}

func TestRegister_ShouldPanicWhenExternalSourceMissingCallerField(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)

	require.Panics(t, func() {
		Register(&Event{ID: "TEST-003", ExternalSource: true})
	})
}

func TestRegister_ShouldStoreValidEvent(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)

	Register(&Event{ID: "TEST-004", Level: slog.LevelInfo, ExternalSource: true, Fields: callerFields()})

	got, ok := Get("TEST-004")
	require.True(t, ok)
	assert.Equal(t, EventID("TEST-004"), got.ID)
}

func TestRegistry_QueriesReflectRegisteredEvents(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)

	Register(&Event{ID: "SYS-900", Level: slog.LevelInfo})
	Register(&Event{ID: "SYS-901", Level: slog.LevelInfo})
	Register(&Event{ID: "API-900", Level: slog.LevelInfo})

	assert.Equal(t, 3, count())
	assert.Len(t, GetAll(), 3)
	assert.Len(t, getByCategory(CategorySystem), 2)
	assert.Len(t, getByCategory(CategoryAPI), 1)
}
