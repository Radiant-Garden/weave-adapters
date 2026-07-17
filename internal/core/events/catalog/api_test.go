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
}
