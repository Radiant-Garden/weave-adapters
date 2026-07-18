/*
Testing: hlt.go

Pending:

Tested:
  init (HLT registration)
    - TestHLTCatalog_ShouldRegisterTransitionEvent: HLT-001 registers with the
      Health category, WARN level, ExternalSource false, and from/to declared as
      required string fields.

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

	// A transition is meaningless without both endpoints, so both are required.
	assert.Equal(
		t,
		[]events.FieldDef{
			{Name: "from", Type: "string", Required: true, Description: "Previous overall status."},
			{Name: "to", Type: "string", Required: true, Description: "New overall status."},
		},
		e.Fields,
	)
}
