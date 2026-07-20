/*
Testing: event.go

Pending:

Tested:
  Impact.String
    - TestImpact_String_ShouldReturnSnakeCase: the one declared impact and the unknown fallback.
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

	tests := []struct {
		impact Impact
		want   string
	}{
		{ImpactRequestRejected, "request_rejected"},
		{Impact(999), "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, tc.impact.String())
		})
	}
}

func TestEventCategory_String_ShouldReturnPrefix(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SYS", CategorySystem.String())
	assert.Equal(t, "API", CategoryAPI.String())
	assert.Equal(t, "HLT", CategoryHealth.String())
}
