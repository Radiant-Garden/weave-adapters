/*
Testing: recorder.go, assertions.go

Pending:

Tested:
  Recorder.Install / capture / FindByID / Data
    - TestRecorder_ShouldCaptureEmittedEvent: an emitted event is captured with its data.
  AssertEmitted / AssertNotEmitted / AssertEmittedN / AssertData
    - TestRecorder_Assertions_ShouldMatchCapturedEvents: the assertion helpers pass on a match.

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
