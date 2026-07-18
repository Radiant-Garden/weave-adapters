/*
Testing: hlt.go

Pending:

Tested:
  init (HLT registration)
    - TestHLTCatalog_ShouldRegisterTransitionEvent: HLT-001 registers with the
      Health category, WARN level, and ExternalSource false.

Tested elsewhere:
  HLT-001 emission: exercised by the health handler tests (internal/core/health).

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

func TestHLTCatalog_ShouldRegisterTransitionEvent(t *testing.T) {
	t.Parallel()

	e, ok := events.Get(HLT001)
	require.True(t, ok, "HLT-001 should be registered")
	assert.Equal(t, events.CategoryHealth.String(), e.Category)
	assert.Equal(t, slog.LevelWarn, e.Level)
	assert.False(t, e.ExternalSource, "health transition is a system event, not request-triggered")
}
