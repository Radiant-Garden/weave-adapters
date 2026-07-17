/*
Testing: sys.go

Pending:

Tested:
  init (SYS registrations)
    - TestSYSCatalog_ShouldRegisterAllLifecycleEvents: the five SYS events register
      without panic, carry the SYS category, and are not ExternalSource.
    - TestSYSCatalog_ShouldFixSeverityPerEvent: SYS-005 is ERROR; SYS-001..004 are INFO.

Tested elsewhere:

Declined:

Additional Remarks:
  Registration happens in init(); the registry is read-only during the test, so
  this is parallel-safe. The ExternalSource: false assertion guards Principle 6 —
  a lifecycle event flipped to true would panic at Emit (the lifecycle context
  carries no remoteAddr).
*/

package catalog

import (
	"log/slog"
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSYSCatalog_ShouldRegisterAllLifecycleEvents(t *testing.T) {
	t.Parallel()

	for _, id := range []events.EventID{SYS001, SYS002, SYS003, SYS004, SYS005} {
		e, ok := events.Get(id)
		require.Truef(t, ok, "event %s should be registered", id)
		assert.Equal(t, events.CategorySystem.String(), e.Category)
		assert.Falsef(t, e.ExternalSource, "lifecycle event %s must not be ExternalSource", id)
	}
}

func TestSYSCatalog_ShouldFixSeverityPerEvent(t *testing.T) {
	t.Parallel()

	wantLevel := map[events.EventID]slog.Level{
		SYS001: slog.LevelInfo,
		SYS002: slog.LevelInfo,
		SYS003: slog.LevelInfo,
		SYS004: slog.LevelInfo,
		SYS005: slog.LevelError,
	}

	for id, want := range wantLevel {
		e, ok := events.Get(id)
		require.Truef(t, ok, "event %s should be registered", id)
		assert.Equalf(t, want, e.Level, "event %s level", id)
	}
}
