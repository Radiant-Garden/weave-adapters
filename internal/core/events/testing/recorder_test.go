/*
Testing: recorder.go, assertions.go

Pending:

Tested:
  Recorder.Install / capture / FindByID / Data
    - TestRecorder_ShouldCaptureEmittedEvent: an emitted event is captured with its data.
  RecordedEvent.Caller / Request
    - TestRecorder_ShouldCaptureCallerAndRequestGroups: an ExternalSource emission
      captures the caller and request groups, decoded by the accessors.
  AssertEmitted / AssertNotEmitted / AssertEmittedN / AssertData
    - TestRecorder_Assertions_ShouldMatchCapturedEvents: the assertion helpers pass on a match.
  AssertMatchesCatalog / matchCatalog / RecordedEvent.has
    - TestRecorder_MatchCatalog_ShouldPassForConformantEmission: a well-formed emission has no problems.
    - TestRecorder_MatchCatalog_ShouldReportDrift: a missing required field and an
      undeclared data field are both reported.
    - TestRecorder_MatchCatalog_ShouldReportUnregistered: an unregistered ID is reported.

Tested elsewhere:

Declined:

Additional Remarks:
  Mutates the global registry and emitter hook, so it runs sequentially. This
  package is named "testing"; import it with an alias from other packages.
*/

package testing

import (
	"context"
	"log/slog"
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_ShouldCaptureEmittedEvent(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	events.Register(&events.Event{ID: "REC-001", Level: slog.LevelInfo, MessageTemplate: "rec"})

	rec := NewRecorder()
	t.Cleanup(rec.Install())

	events.Emit(context.Background(), "REC-001", "key", "value")

	found := rec.FindByID("REC-001")
	if len(found) != 1 {
		t.Fatalf("expected 1 captured event, got %d", len(found))
	}

	if got := found[0].Data("key"); got != "value" {
		t.Errorf("Data(key) = %v, want value", got)
	}
}

func TestRecorder_Assertions_ShouldMatchCapturedEvents(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	events.Register(&events.Event{ID: "REC-002", Level: slog.LevelInfo, MessageTemplate: "rec"})

	rec := NewRecorder()
	t.Cleanup(rec.Install())

	events.Emit(context.Background(), "REC-002", "key", "value")

	rec.AssertEmitted(t, "REC-002")
	rec.AssertEmittedN(t, "REC-002", 1)
	rec.AssertData(t, "REC-002", "key", "value")
	rec.AssertNotEmitted(t, "REC-404")
}

func TestRecorder_ShouldCaptureCallerAndRequestGroups(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	events.Register(&events.Event{
		ID: "REC-003", Level: slog.LevelInfo, MessageTemplate: "rec", ExternalSource: true,
		Fields: []events.FieldDef{
			{Name: "subject", Type: "string"},
			{Name: "role", Type: "string"},
			{Name: "remoteAddr", Type: "string"},
		},
	})

	rec := NewRecorder()
	t.Cleanup(rec.Install())

	ctx := events.WithCaller(context.Background(), events.Caller{
		Subject: "svc", Role: "admin", RemoteAddr: "1.2.3.4", RequestID: "req-1", Method: "GET", Path: "/x",
	})
	events.Emit(ctx, "REC-003", "key", "value")

	found := rec.FindByID("REC-003")
	require.Len(t, found, 1)
	assert.Equal(t, "svc", found[0].Caller("subject"))
	assert.Equal(t, "req-1", found[0].Request("requestId"))
	assert.Equal(t, "value", found[0].Data("key"))
}

func TestRecorder_MatchCatalog_ShouldPassForConformantEmission(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	events.Register(&events.Event{
		ID: "REC-010", Level: slog.LevelInfo, MessageTemplate: "rec",
		Fields: []events.FieldDef{{Name: "version", Type: "string", Required: true}},
	})

	rec := NewRecorder()
	t.Cleanup(rec.Install())

	events.Emit(context.Background(), "REC-010", "version", "1.2.3")

	assert.Empty(t, rec.matchCatalog())
}

func TestRecorder_MatchCatalog_ShouldReportDrift(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	events.Register(&events.Event{
		ID: "REC-011", Level: slog.LevelInfo, MessageTemplate: "rec",
		Fields: []events.FieldDef{{Name: "version", Type: "string", Required: true}},
	})

	rec := NewRecorder()
	t.Cleanup(rec.Install())

	// Missing the required "version" and carrying an undeclared "bogus".
	events.Emit(context.Background(), "REC-011", "bogus", "x")

	problems := rec.matchCatalog()
	require.Len(t, problems, 2)

	joined := problems[0].Error() + "\n" + problems[1].Error()
	assert.Contains(t, joined, `missing required field "version"`)
	assert.Contains(t, joined, `undeclared data field "bogus"`)
}

func TestRecorder_MatchCatalog_ShouldReportUnregistered(t *testing.T) { //nolint:paralleltest // mutates global hook
	rec := NewRecorder()
	t.Cleanup(rec.Install())

	events.Emit(context.Background(), "REC-UNREGISTERED", "k", "v")

	problems := rec.matchCatalog()
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0].Error(), "not registered")
	assert.NotContains(t, problems[0].Error(), "required") // stops after the nil-spec check
}
