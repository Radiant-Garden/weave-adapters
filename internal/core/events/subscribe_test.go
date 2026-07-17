/*
Testing: subscribe.go

Pending:

Tested:
  Subscribe / fanOutToSubscribers
    - TestSubscribe_ShouldReceiveMatchingEvents: a matching event is delivered with its fields.
    - TestSubscribe_ShouldFilterNonMatchingEvents: a non-matching event is not delivered.
    - TestSubscribe_ShouldDropWhenBufferFull: a slow consumer drops rather than blocking Emit.
    - TestSubscribe_UnsubscribeStopsDelivery: after unsubscribe no more events arrive.

Tested elsewhere:

Declined:

Additional Remarks:
  Uses the global subscriber list and registry, so tests run sequentially and
  clean up their subscription and registry.
*/

package events

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_ShouldReceiveMatchingEvents(t *testing.T) { //nolint:paralleltest // mutates global registry/subscribers
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-030", Level: slog.LevelInfo})

	sub, unsub := Subscribe(4, func(id EventID) bool { return id == "TEST-030" })
	t.Cleanup(unsub)

	Emit(context.Background(), "TEST-030", "k", "v")

	select {
	case e := <-sub.Ch():
		assert.Equal(t, EventID("TEST-030"), e.ID)
		assert.Equal(t, "v", e.Fields["k"])
	default:
		t.Fatal("expected an event, got none")
	}
}

func TestSubscribe_ShouldFilterNonMatchingEvents(t *testing.T) { //nolint:paralleltest // mutates global registry/subscribers
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-031", Level: slog.LevelInfo})

	sub, unsub := Subscribe(4, func(id EventID) bool { return id == "OTHER" })
	t.Cleanup(unsub)

	Emit(context.Background(), "TEST-031")

	select {
	case <-sub.Ch():
		t.Fatal("did not expect a non-matching event")
	default:
	}
}

func TestSubscribe_ShouldDropWhenBufferFull(t *testing.T) { //nolint:paralleltest // mutates global registry/subscribers
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-032", Level: slog.LevelInfo})

	sub, unsub := Subscribe(1, func(id EventID) bool { return id == "TEST-032" })
	t.Cleanup(unsub)

	// Emit three; only one fits the buffer, the rest drop without blocking.
	require.NotPanics(t, func() {
		Emit(context.Background(), "TEST-032")
		Emit(context.Background(), "TEST-032")
		Emit(context.Background(), "TEST-032")
	})

	assert.Len(t, sub.Ch(), 1)
}

func TestSubscribe_UnsubscribeStopsDelivery(t *testing.T) { //nolint:paralleltest // mutates global registry/subscribers
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-033", Level: slog.LevelInfo})

	sub, unsub := Subscribe(4, func(id EventID) bool { return id == "TEST-033" })
	unsub()

	Emit(context.Background(), "TEST-033")

	select {
	case <-sub.Ch():
		t.Fatal("expected no delivery after unsubscribe")
	default:
	}
}
