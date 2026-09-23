/*
Testing: api.go

Pending:

Tested:
  init (API registrations)
    - TestAPICatalog_ShouldRegisterRequestEvents: API-010/011 register with the
      expected category, level, and ExternalSource setting.

Tested elsewhere:
  API-010 / API-011 emission: exercised by the middleware tests
    (internal/core/middleware).

Declined:

Additional Remarks:
  Registration happens in init(); the registry is read-only during the test, so
  this is parallel-safe.
*/

package catalog

import (
	"log/slog"
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPICatalog_ShouldRegisterRequestEvents(t *testing.T) {
	t.Parallel()

	completed, ok := events.Get(API010)
	require.True(t, ok, "API-010 should be registered")
	assert.Equal(t, events.CategoryAPI.String(), completed.Category)
	assert.Equal(t, slog.LevelInfo, completed.Level)
	assert.True(t, completed.ExternalSource, "request-completed is request-triggered")

	panicked, ok := events.Get(API011)
	require.True(t, ok, "API-011 should be registered")
	assert.Equal(t, slog.LevelError, panicked.Level)
	assert.False(t, panicked.ExternalSource, "panic event runs in outermost recovery, no caller context")

	// The stack is declared, so the log FILE carries it — and marked
	// EventLogOmit, so the Windows Event Log does not. That log is readable by
	// every local user, and Recovery is the outermost middleware, so it runs
	// before authentication: an unauthenticated request that panicked a
	// handler would otherwise publish package paths, file names and line
	// numbers to anyone with a console.
	stack, found := fieldNamed(panicked, "stack")
	require.True(t, found, "API-011 must still carry the stack; it is how a panic is diagnosed")
	assert.True(t, stack.EventLogOmit, "the stack must not reach the Windows Event Log")

	// The bound is as important as the flag: everything else on this event is
	// what makes the entry worth opening Event Viewer for.
	for _, name := range []string{"method", "path", "remoteAddr", "requestId", "panic"} {
		f, ok := fieldNamed(panicked, name)
		require.True(t, ok, "API-011 should declare %s", name)
		assert.False(t, f.EventLogOmit, "%s belongs in the Event Log entry", name)
	}
}

// fieldNamed returns an event's field definition by name.
func fieldNamed(event *events.Event, name string) (events.FieldDef, bool) {
	for _, f := range event.Fields {
		if f.Name == name {
			return f, true
		}
	}

	return events.FieldDef{}, false
}
