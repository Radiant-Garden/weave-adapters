/*
Testing: logging.go

Pending:

Tested:
  newHandler / levelFor
    - TestNewHandler_ShouldEnableLevelFromSeverity: severity string maps to the slog level,
      including the unknown-string -> info fallback.
  Setup
    - TestSetup_ShouldWriteToTheLogFileWhenOneIsConfigured: the whole reason the
      key exists -- a service has no console and the SCM discards stdout.
    - TestSetup_ShouldAppendRatherThanTruncate: a restart must not erase the log
      of the run that prompted it.
    - TestSetup_ShouldReportAnUnopenableLogFile: the one failure that cannot
      report itself through the log, so it has to come back as an error.
    - TestSetup_ShouldReturnANoOpCloserWhenItOpenedNothing: closing stdout would
      be worse than doing nothing.
    - TestSetup_ShouldWriteToEverySink: the fan-out reaches the Event Log arm.

Tested elsewhere:
  That events reach the configured sink at all: the events tests and the
  running binary.

Declined:

Additional Remarks:
  Setup mutates the process-global slog default, so its tests cannot run in
  parallel and restore it in t.Cleanup. The severity table above does not touch
  the global -- it builds a handler directly -- so it stays parallel.
*/

package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandler_ShouldEnableLevelFromSeverity(t *testing.T) {
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
			logger := slog.New(newHandler(io.Discard, tc.severity))

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

// restoreDefault captures the process logger and puts it back afterwards, so a
// Setup test cannot leak its sink into whatever runs next.
func restoreDefault(t *testing.T) {
	t.Helper()

	original := slog.Default()

	t.Cleanup(func() { slog.SetDefault(original) })
}

//nolint:paralleltest // Setup installs the process-global slog default
func TestSetup_ShouldWriteToTheLogFileWhenOneIsConfigured(t *testing.T) {
	// ARRANGE
	restoreDefault(t)

	path := filepath.Join(t.TempDir(), "adapter.log")

	// ACT
	logger, closer, err := Setup("info", path)
	require.NoError(t, err)

	logger.Info("hello from the service")
	require.NoError(t, closer.Close())

	// ASSERT
	// The whole reason logFile exists: under the SCM there is no console and
	// stdout is discarded, so a server that logged only there logs nowhere.
	//nolint:gosec // G304: the path is inside this test's own TempDir.
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "hello from the service")
}

//nolint:paralleltest // Setup installs the process-global slog default
func TestSetup_ShouldAppendRatherThanTruncate(t *testing.T) {
	// ARRANGE
	restoreDefault(t)

	path := filepath.Join(t.TempDir(), "adapter.log")

	first, closer, err := Setup("info", path)
	require.NoError(t, err)

	first.Info("from the first run")
	require.NoError(t, closer.Close())

	// ACT — a second process against the same path, which is what a restart is.
	second, closer2, err := Setup("info", path)
	require.NoError(t, err)

	second.Info("from the second run")
	require.NoError(t, closer2.Close())

	// ASSERT
	// A restart must not erase the log of the run that prompted it, which is
	// precisely the one an operator is about to read.
	//nolint:gosec // G304: the path is inside this test's own TempDir.
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(written), "from the first run")
	assert.Contains(t, string(written), "from the second run")
}

//nolint:paralleltest // Setup installs the process-global slog default
func TestSetup_ShouldReportAnUnopenableLogFile(t *testing.T) {
	// ARRANGE — a path whose parent is a file, so the open cannot succeed.
	restoreDefault(t)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// ACT
	logger, closer, err := Setup("info", filepath.Join(blocker, "adapter.log"))

	// ASSERT
	// Returned, never logged: a log sink that could not be opened is the one
	// error that cannot report itself through the log.
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Nil(t, closer)
	assert.Contains(t, err.Error(), "opening log file")
}

//nolint:paralleltest // Setup installs the process-global slog default
func TestSetup_ShouldReturnANoOpCloserWhenItOpenedNothing(t *testing.T) {
	// ARRANGE
	restoreDefault(t)

	// ACT
	_, closer, err := Setup("info", "")
	require.NoError(t, err)

	// ASSERT
	// Closing stdout would be worse than doing nothing, so the empty case has
	// to hand back something inert rather than the sink itself.
	require.NotNil(t, closer)
	assert.NoError(t, closer.Close())
	assert.NoError(t, closer.Close(), "closing twice must stay harmless")
}

//nolint:paralleltest // Setup installs the process-global slog default
func TestSetup_ShouldWriteToEverySink(t *testing.T) {
	// ARRANGE
	restoreDefault(t)

	path := filepath.Join(t.TempDir(), "adapter.log")

	var extra strings.Builder

	// A second sink at a *higher* severity than the primary, which is the
	// Event Log arm's shape: it takes a narrower slice of the same stream.
	sink := slog.NewTextHandler(&extra, &slog.HandlerOptions{Level: slog.LevelError})

	// ACT
	logger, closer, err := Setup("debug", path, sink)
	require.NoError(t, err)

	logger.Info("only the file wants this")
	logger.Error("both sinks want this")
	require.NoError(t, closer.Close())

	// ASSERT
	//nolint:gosec // G304: the path is inside this test's own TempDir.
	written, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Contains(t, string(written), "only the file wants this")
	assert.Contains(t, string(written), "both sinks want this")

	assert.NotContains(t, extra.String(), "only the file wants this",
		"the sink's own level must still filter it")
	assert.Contains(t, extra.String(), "both sinks want this")
}
