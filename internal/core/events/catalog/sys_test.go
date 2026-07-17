/*
Testing: sys.go

Pending:

Tested:
  init (SYS registrations)
    - TestSYSCatalog_ShouldRegisterAllLifecycleEvents: the four SYS events register
      without panic and carry the SYS category.

Tested elsewhere:

Declined:

Additional Remarks:
  Registration happens in init(); the registry is read-only during the test, so
  this is parallel-safe.
*/

package catalog

import (
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSYSCatalog_ShouldRegisterAllLifecycleEvents(t *testing.T) {
	t.Parallel()

	for _, id := range []events.EventID{SYS001, SYS002, SYS003, SYS004} {
		e, ok := events.Get(id)
		require.Truef(t, ok, "event %s should be registered", id)
		assert.Equal(t, events.CategorySystem.String(), e.Category)
	}
}
