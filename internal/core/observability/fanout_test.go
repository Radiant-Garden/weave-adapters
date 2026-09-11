/*
Testing: fanout.go

Pending:

Tested:

	newFanout -> - TestNewFanout_ShouldUnwrapASingleHandler: the console path
	               stays exactly the handler it was before this type existed.
	Enabled   -> - TestFanoutEnabled_ShouldOrItsChildren: gating on the first
	               child would silence the Event Log arm at logSeverity=error.
	Handle    -> - TestFanoutHandle_ShouldWriteToEveryEnabledChild
	             - TestFanoutHandle_ShouldSkipAChildThatDeclinesTheLevel
	             - TestFanoutHandle_ShouldJoinEveryChildError: a failing sink
	               must not hide another failing sink.
	             - TestFanoutHandle_ShouldGiveEachChildItsOwnRecord: handlers may
	               retain or mutate what they are handed.
	WithAttrs / WithGroup -> - TestFanoutWithAttrs_ShouldApplyToEveryChild
	                         - TestFanoutWithGroup_ShouldApplyToEveryChild

Tested elsewhere:

	That Setup composes this correctly for the real sinks:
	TestSetup_ShouldWriteToEverySink in logging_test.go.

Declined:

	Benchmarking the clone in Handle. The fan-out runs at most twice per record
	and only in the service deployment; a record clone is not where this
	process spends anything worth measuring.

Additional Remarks:

	The child used here records what it was given rather than formatting it, so
	the assertions are about routing rather than about slog's text output --
	which is slog's to test, not ours.
*/
package observability

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capture is a slog.Handler that records what it was handed.
type capture struct {
	level   slog.Level
	err     error
	records []slog.Record
	attrs   []slog.Attr
	groups  []string
}

func (c *capture) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.records = append(c.records, r)

	return c.err
}

func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	c.attrs = append(c.attrs, attrs...)

	return c
}

func (c *capture) WithGroup(name string) slog.Handler {
	c.groups = append(c.groups, name)

	return c
}

// record builds a record at the given level.
func record(level slog.Level, msg string) slog.Record {
	return slog.NewRecord(time.Time{}, level, msg, 0)
}

func TestNewFanout_ShouldUnwrapASingleHandler(t *testing.T) {
	t.Parallel()

	// ARRANGE
	only := &capture{}

	// ACT
	got := newFanout(only)

	// ASSERT
	// Not an optimisation: the console path must be byte-identical to what it
	// was before this type existed.
	assert.Same(t, slog.Handler(only), got)
}

func TestFanoutEnabled_ShouldOrItsChildren(t *testing.T) {
	t.Parallel()

	// ARRANGE — the file at error, a second sink at info.
	h := newFanout(&capture{level: slog.LevelError}, &capture{level: slog.LevelInfo})

	// ACT / ASSERT
	// Gating on the first child is the bug this guards: an operator setting
	// logSeverity=error wants less noise in the file, not a silent Event Log.
	assert.True(t, h.Enabled(t.Context(), slog.LevelInfo))
	assert.True(t, h.Enabled(t.Context(), slog.LevelError))
	assert.False(t, h.Enabled(t.Context(), slog.LevelDebug))
}

func TestFanoutHandle_ShouldWriteToEveryEnabledChild(t *testing.T) {
	t.Parallel()

	// ARRANGE
	a, b := &capture{level: slog.LevelInfo}, &capture{level: slog.LevelInfo}
	h := newFanout(a, b)

	// ACT
	require.NoError(t, h.Handle(t.Context(), record(slog.LevelInfo, "both")))

	// ASSERT
	require.Len(t, a.records, 1)
	require.Len(t, b.records, 1)
	assert.Equal(t, "both", a.records[0].Message)
	assert.Equal(t, "both", b.records[0].Message)
}

func TestFanoutHandle_ShouldSkipAChildThatDeclinesTheLevel(t *testing.T) {
	t.Parallel()

	// ARRANGE
	file, eventLog := &capture{level: slog.LevelDebug}, &capture{level: slog.LevelError}
	h := newFanout(file, eventLog)

	// ACT
	require.NoError(t, h.Handle(t.Context(), record(slog.LevelInfo, "file only")))

	// ASSERT
	// The narrower sink is the Event Log arm's shape -- it takes a slice of the
	// same stream, and Enabled on the fan-out said yes because the file wanted it.
	assert.Len(t, file.records, 1)
	assert.Empty(t, eventLog.records)
}

func TestFanoutHandle_ShouldJoinEveryChildError(t *testing.T) {
	t.Parallel()

	// ARRANGE
	errA, errB := errors.New("sink a failed"), errors.New("sink b failed")
	h := newFanout(
		&capture{level: slog.LevelInfo, err: errA},
		&capture{level: slog.LevelInfo, err: errB},
	)

	// ACT
	err := h.Handle(t.Context(), record(slog.LevelInfo, "x"))

	// ASSERT
	// First-wins would let a broken Event Log arm hide a broken file arm.
	require.Error(t, err)
	require.ErrorIs(t, err, errA)
	require.ErrorIs(t, err, errB)
}

func TestFanoutHandle_ShouldGiveEachChildItsOwnRecord(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// TEN ATTRIBUTES, AND BOTH CHILDREN APPEND. Neither detail is incidental,
	// and getting them wrong is how the first version of this test passed
	// against a fanout that did not clone at all:
	//
	//   - slog.Record keeps its first five attributes inline and is passed by
	//     value, so with five or fewer, ordinary copy semantics already isolate
	//     the children and a missing Clone is invisible.
	//   - one child appending is not enough either. A copy that never grows
	//     never reads past its own length, so it cannot observe the other's
	//     write. The shared overflow slice only bites when both append.
	//
	// slog detects this rather than corrupting silently: it appends a "!BUG"
	// attribute naming the mistake. That is the observable defect -- a !BUG
	// entry in an operator's log line -- so that is what this asserts.
	a, b := &capture{level: slog.LevelInfo}, &capture{level: slog.LevelInfo}
	h := newFanout(a, b)

	rec := record(slog.LevelInfo, "shared")
	for i := range 10 {
		rec.AddAttrs(slog.Int("original", i))
	}

	// ACT
	require.NoError(t, h.Handle(t.Context(), rec))

	require.Len(t, a.records, 1)
	require.Len(t, b.records, 1)

	// A handler is allowed to retain and mutate what it was handed, which is
	// exactly what WithAttrs-style enrichment does.
	a.records[0].AddAttrs(slog.String("added-by-a", "1"))
	b.records[0].AddAttrs(slog.String("added-by-b", "2"))

	// ASSERT
	assert.False(t, hasBugAttr(a.records[0]), "child a's record carries slog's !BUG marker")
	assert.False(t, hasBugAttr(b.records[0]), "child b's record carries slog's !BUG marker")
}

// hasBugAttr reports whether slog flagged the record as a copy that two
// handlers both mutated. slog appends an attribute whose key is "!BUG" rather
// than panicking, so without it the only symptom is a corrupted log line.
func hasBugAttr(r slog.Record) bool {
	found := false

	r.Attrs(func(a slog.Attr) bool {
		if strings.Contains(a.Key, "BUG") {
			found = true

			return false
		}

		return true
	})

	return found
}

func TestFanoutWithAttrs_ShouldApplyToEveryChild(t *testing.T) {
	t.Parallel()

	// ARRANGE
	a, b := &capture{level: slog.LevelInfo}, &capture{level: slog.LevelInfo}
	h := newFanout(a, b)

	// ACT
	_ = h.WithAttrs([]slog.Attr{slog.String("service", "adapter")})

	// ASSERT
	assert.Len(t, a.attrs, 1)
	assert.Len(t, b.attrs, 1)
}

func TestFanoutWithGroup_ShouldApplyToEveryChild(t *testing.T) {
	t.Parallel()

	// ARRANGE
	a, b := &capture{level: slog.LevelInfo}, &capture{level: slog.LevelInfo}
	h := newFanout(a, b)

	// ACT
	_ = h.WithGroup("data")

	// ASSERT
	assert.Equal(t, []string{"data"}, a.groups)
	assert.Equal(t, []string{"data"}, b.groups)
}
