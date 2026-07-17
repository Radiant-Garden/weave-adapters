/*
Testing: event.go

Pending:

Tested:
  Impact.String
    - TestImpact_String_ShouldReturnSnakeCase: each impact and the unknown fallback.
  EventCategory.String
    - TestEventCategory_String_ShouldReturnPrefix: category prefix string.

Tested elsewhere:

Declined:

Additional Remarks:
*/

package events

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImpact_String_ShouldReturnSnakeCase(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "request_rejected", ImpactRequestRejected.String())
	assert.Equal(t, "state_changed", ImpactStateChanged.String())
	assert.Equal(t, "resource_deleted", ImpactResourceDeleted.String())
	assert.Equal(t, "unknown", Impact(999).String())
}

func TestEventCategory_String_ShouldReturnPrefix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SYS", CategorySystem.String())
	assert.Equal(t, "API", CategoryAPI.String())
	assert.Equal(t, "HLT", CategoryHealth.String())
}
