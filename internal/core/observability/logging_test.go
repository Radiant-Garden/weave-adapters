/*
Testing: logging.go

Pending:

Tested:
  newLogger / levelFor
    - TestNewLogger_ShouldEnableLevelFromSeverity: severity string maps to the slog level,
      including the unknown-string -> info fallback.

Tested elsewhere:
  Setup: installs the slog default; exercised end-to-end via the events tests
    and the running binary. Not unit-tested here because it mutates global slog
    state.

Declined:

Additional Remarks:
*/

package observability

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewLogger_ShouldEnableLevelFromSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		severity      string
		enabled       slog.Level
		notEnabled    slog.Level
		checkNotAbove bool
	}{
		{severity: "debug", enabled: slog.LevelDebug},
		{severity: "info", enabled: slog.LevelInfo, notEnabled: slog.LevelDebug, checkNotAbove: true},
		{severity: "warn", enabled: slog.LevelWarn, notEnabled: slog.LevelInfo, checkNotAbove: true},
		{severity: "error", enabled: slog.LevelError, notEnabled: slog.LevelWarn, checkNotAbove: true},
		{severity: "bogus", enabled: slog.LevelInfo, notEnabled: slog.LevelDebug, checkNotAbove: true},
	}

	for _, tc := range tests {
		t.Run(tc.severity, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			logger := newLogger(tc.severity)

			// ASSERT
			assert.True(t, logger.Enabled(context.Background(), tc.enabled),
				"level %v should be enabled at severity %q", tc.enabled, tc.severity)

			if tc.checkNotAbove {
				assert.False(t, logger.Enabled(context.Background(), tc.notEnabled),
					"level %v should be disabled at severity %q", tc.notEnabled, tc.severity)
			}
		})
	}
}
