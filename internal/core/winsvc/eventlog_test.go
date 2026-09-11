/*
Testing: eventlog.go

Pending:

Tested:

	eventLogIDOf -> - TestEventLogIDOf_ShouldReadTheNumberFromTheRegistry
	                - TestEventLogIDOf_ShouldDropARecordTheCatalogDoesNotMirror
	                - TestEventLogIDOf_ShouldDropARawSlogCall: breadcrumbs stay
	                  out of an operator's Event Viewer.
	Enabled      -> - TestEventLogHandlerEnabled_ShouldFloorAtInfo: the fan-out
	                  ORs its children, so answering true everywhere would build
	                  a record for every breadcrumb in the process.
	Handle       -> - TestEventLogHandlerHandle_ShouldRouteBySeverity
	                - TestEventLogHandlerHandle_ShouldWriteNothingForAnUnmirroredEvent
	                - TestEventLogHandlerHandle_ShouldReportAWriteFailure
	render       -> - TestEventLogHandlerHandle_ShouldFlattenGroupsIntoTheMessage
	                - TestEventLogHandlerHandle_ShouldElideAnEmptyGroup
	WithAttrs    -> - TestEventLogHandlerWithAttrs_ShouldCarryThemOntoEveryEntry
	WithGroup    -> - TestEventLogHandlerWithGroup_ShouldReturnTheHandlerUnchanged

Tested elsewhere:

	Which events are eligible, and that every eligible one has a number:
	internal/catalogs, the only package that links every catalog. Register
	itself panics on a duplicate, an out-of-range ID, and an ExternalSource
	event claiming one.

	That the entries actually render in Event Viewer: M4a Phase -1 measured the
	ID ceiling on Windows Server 2022, and task service-gate asserts a real
	SYS-001 is readable after an install.

Declined:

	Testing eventlog.Open, Install and Remove. They are three calls into
	x/sys that build only on Windows and do nothing a fake could observe; the
	gate covers them against a real registry.

Additional Remarks:

	These tests register throwaway events in the global registry, so they
	cannot run in parallel with anything that walks it. The IDs are outside
	every allocated range so a mistake here cannot look like a real event.
*/
package winsvc

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// entry is one write the fake sink recorded.
type entry struct {
	level string
	eid   uint32
	msg   string
}

// fakeEventLog records what it was asked to write.
type fakeEventLog struct {
	entries []entry
	err     error
}

func (f *fakeEventLog) info(eid uint32, msg string) error {
	f.entries = append(f.entries, entry{"info", eid, msg})

	return f.err
}

func (f *fakeEventLog) warning(eid uint32, msg string) error {
	f.entries = append(f.entries, entry{"warning", eid, msg})

	return f.err
}

func (f *fakeEventLog) error(eid uint32, msg string) error {
	f.entries = append(f.entries, entry{"error", eid, msg})

	return f.err
}

func (f *fakeEventLog) Close() error { return nil }

// testEvent IDs sit outside every allocated range, so a leak into a real
// assertion is obvious rather than plausible.
const (
	mirroredID   events.EventID = "TEST-901"
	unmirroredID events.EventID = "TEST-902"
)

// registerTestEvents puts two throwaway events in the registry: one mirrored to
// the Event Log, one not.
func registerTestEvents(t *testing.T) {
	t.Helper()

	if _, already := events.Get(mirroredID); already {
		return
	}

	events.Register(&events.Event{
		ID:              mirroredID,
		Level:           slog.LevelWarn,
		EventLogID:      997,
		MessageTemplate: "mirrored test event",
		Description:     "Registered by winsvc's tests.",
		Category:        "TEST",
		Topic:           "Testing",
	})

	events.Register(&events.Event{
		ID:              unmirroredID,
		Level:           slog.LevelWarn,
		MessageTemplate: "unmirrored test event",
		Description:     "Registered by winsvc's tests.",
		Category:        "TEST",
		Topic:           "Testing",
	})
}

// mirroredRecord builds a record the way Emit does: the catalog ID as an
// attribute, the rest inside a "data" group.
func mirroredRecord(level slog.Level, id events.EventID, data ...any) slog.Record {
	r := slog.NewRecord(time.Time{}, level, "something happened", 0)
	r.Add(eventIDAttr, string(id))

	if len(data) > 0 {
		r.AddAttrs(slog.Group("data", data...))
	}

	return r
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogIDOf_ShouldReadTheNumberFromTheRegistry(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	// ACT
	eid, mirrored := eventLogIDOf(mirroredRecord(slog.LevelWarn, mirroredID))

	// ASSERT
	// Read back from the catalog rather than from a table here, so the sink
	// cannot drift from what the owning package declared.
	assert.True(t, mirrored)
	assert.Equal(t, uint32(997), eid)
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogIDOf_ShouldDropARecordTheCatalogDoesNotMirror(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	// ACT
	_, mirrored := eventLogIDOf(mirroredRecord(slog.LevelWarn, unmirroredID))

	// ASSERT
	assert.False(t, mirrored)
}

//nolint:paralleltest // reads the process-global event registry
func TestEventLogIDOf_ShouldDropARawSlogCall(t *testing.T) {
	// ARRANGE — no eventId attribute at all.
	r := slog.NewRecord(time.Time{}, slog.LevelError, "a raw slog call", 0)

	// ACT
	_, mirrored := eventLogIDOf(r)

	// ASSERT
	// slog.Debug is for developer breadcrumbs by convention, and an operator's
	// Event Viewer is not where they belong.
	assert.False(t, mirrored)
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerHandle_ShouldRouteBySeverity(t *testing.T) {
	registerTestEvents(t)

	tests := map[string]struct {
		level slog.Level
		want  string
	}{
		"should write info for an info record":   {level: slog.LevelInfo, want: "info"},
		"should write warning for a warn record": {level: slog.LevelWarn, want: "warning"},
		"should write error for an error record": {level: slog.LevelError, want: "error"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// ARRANGE
			f := &fakeEventLog{}
			h := &eventLogHandler{w: f}

			// ACT
			require.NoError(t, h.Handle(t.Context(), mirroredRecord(tc.level, mirroredID)))

			// ASSERT
			// Event Viewer's own severity column, which is what an operator
			// filters on before they read anything.
			require.Len(t, f.entries, 1)
			assert.Equal(t, tc.want, f.entries[0].level)
			assert.Equal(t, uint32(997), f.entries[0].eid)
		})
	}
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerHandle_ShouldWriteNothingForAnUnmirroredEvent(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	f := &fakeEventLog{}
	h := &eventLogHandler{w: f}

	// ACT
	require.NoError(t, h.Handle(t.Context(), mirroredRecord(slog.LevelWarn, unmirroredID)))

	// ASSERT
	// The Event Log is the short list, not a second copy of the log.
	assert.Empty(t, f.entries)
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerHandle_ShouldReportAWriteFailure(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	wantErr := errors.New("the event log is full")
	h := &eventLogHandler{w: &fakeEventLog{err: wantErr}}

	// ACT
	err := h.Handle(t.Context(), mirroredRecord(slog.LevelError, mirroredID))

	// ASSERT
	// slog discards this, but the fan-out joins it, so a test can see that a
	// failing Event Log arm is not silently swallowed here.
	require.ErrorIs(t, err, wantErr)
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerHandle_ShouldFlattenGroupsIntoTheMessage(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	f := &fakeEventLog{}
	h := &eventLogHandler{w: f}

	// ACT
	require.NoError(t, h.Handle(t.Context(),
		mirroredRecord(slog.LevelWarn, mirroredID, "runMode", "service", "version", "1.2.3")))

	// ASSERT
	// An Event Log entry is one flat string, so the group has to become dotted
	// keys or the values an operator needs are simply absent.
	require.Len(t, f.entries, 1)

	msg := f.entries[0].msg
	assert.Contains(t, msg, "something happened")
	assert.Contains(t, msg, "data.runMode=service")
	assert.Contains(t, msg, "data.version=1.2.3")
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerHandle_ShouldElideAnEmptyGroup(t *testing.T) {
	// ARRANGE — SYS-004's shape: an event with no data fields.
	registerTestEvents(t)

	f := &fakeEventLog{}
	h := &eventLogHandler{w: f}

	// ACT
	require.NoError(t, h.Handle(t.Context(), mirroredRecord(slog.LevelWarn, mirroredID)))

	// ASSERT
	require.Len(t, f.entries, 1)
	assert.NotContains(t, f.entries[0].msg, "data=")
}

//nolint:paralleltest // registers into the process-global event registry
func TestEventLogHandlerWithAttrs_ShouldCarryThemOntoEveryEntry(t *testing.T) {
	// ARRANGE
	registerTestEvents(t)

	f := &fakeEventLog{}
	h := (&eventLogHandler{w: f}).WithAttrs([]slog.Attr{slog.String("host", "dhcp01")})

	// ACT
	require.NoError(t, h.Handle(t.Context(), mirroredRecord(slog.LevelWarn, mirroredID)))

	// ASSERT
	require.Len(t, f.entries, 1)
	assert.Contains(t, f.entries[0].msg, "host=dhcp01")
}

//nolint:paralleltest // reads the process-global event registry
func TestEventLogHandlerWithGroup_ShouldReturnTheHandlerUnchanged(t *testing.T) {
	// ARRANGE
	h := &eventLogHandler{w: &fakeEventLog{}}

	// ACT
	got := h.WithGroup("data")

	// ASSERT
	// The entry is a flat string, so there is no nesting to express, and
	// prefixing keys would change text an operator reads directly.
	assert.Same(t, slog.Handler(h), got)
}

func TestEventLogHandlerEnabled_ShouldFloorAtInfo(t *testing.T) {
	t.Parallel()

	// ARRANGE
	h := &eventLogHandler{w: &fakeEventLog{}}

	// ACT / ASSERT
	// The floor exists because the fan-out ORs its children: answering true at
	// every level would have slog build a record for every breadcrumb in the
	// process, whatever logSeverity said, purely so this handler could drop it.
	// INFO is safe because no DEBUG event can carry an EventLogID -- the
	// conformance test in internal/catalogs rejects one.
	assert.False(t, h.Enabled(t.Context(), slog.LevelDebug))
	assert.True(t, h.Enabled(t.Context(), slog.LevelInfo))
	assert.True(t, h.Enabled(t.Context(), slog.LevelWarn))
	assert.True(t, h.Enabled(t.Context(), slog.LevelError))
}
