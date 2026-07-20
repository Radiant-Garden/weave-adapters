/*
Testing: emit.go

Pending:

Tested:
  Emit
    - TestEmit_ShouldWrapDataInGroup: internal event attaches a "data" group.
    - TestEmit_ShouldPanicWhenExternalSourceMissingRemoteAddr: guard against background ctx.
    - TestEmit_ShouldAttachCallerAndRequestWhenExternalSource: caller/request groups from ctx.
    - TestEmit_ShouldWarnOnUnknownID: unregistered ID still calls the hook, no panic.
    - TestEmit_ShouldBeSafeForConcurrentEmitters: N goroutines emit concurrently with a
      hook attached; validates the global hook locking under -race.

Tested elsewhere:

Declined:

Additional Remarks:
  These tests mutate the global registry and emitter hook, so they run
  sequentially (withCleanRegistry restores the registry; the hook is restored in
  t.Cleanup).
*/

package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHook installs an emitter hook recording the attrs for the given ID and
// restores the previous hook on cleanup.
func captureHook(t *testing.T, want EventID, into *[]any) {
	t.Helper()

	prev := SetEmitterHook(func(id EventID, attrs ...any) {
		if id == want {
			*into = attrs
		}
	})

	t.Cleanup(func() { SetEmitterHook(prev) })
}

// groupField extracts a field value from a captured slog group attribute.
func groupField(t *testing.T, attr any, wantGroup, key string) any {
	t.Helper()

	a, ok := attr.(slog.Attr)
	require.True(t, ok, "attr is not an slog.Attr")
	require.Equal(t, wantGroup, a.Key)

	for _, sub := range a.Value.Group() {
		if sub.Key == key {
			return sub.Value.Any()
		}
	}

	return nil
}

func TestEmit_ShouldWrapDataInGroup(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-010", Level: slog.LevelInfo, MessageTemplate: "x"})

	var got []any

	captureHook(t, "TEST-010", &got)

	Emit(context.Background(), "TEST-010", "key", "value")

	require.Len(t, got, 1)
	assert.Equal(t, "value", groupField(t, got[0], "data", "key"))
}

func TestEmit_ShouldPanicWhenExternalSourceMissingRemoteAddr(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-011", Level: slog.LevelInfo, ExternalSource: true, Fields: callerFields()})

	require.Panics(t, func() {
		Emit(context.Background(), "TEST-011")
	})
}

func TestEmit_ShouldAttachCallerAndRequestWhenExternalSource(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-012", Level: slog.LevelInfo, ExternalSource: true, Fields: callerFields()})

	var got []any

	captureHook(t, "TEST-012", &got)

	ctx := WithCaller(context.Background(), Caller{
		Subject: "svc", Role: "admin", RemoteAddr: "1.2.3.4", RequestID: "req-1", Method: "GET", Path: "/x",
	})
	Emit(ctx, "TEST-012", "key", "value")

	require.Len(t, got, 3)
	assert.Equal(t, "svc", groupField(t, got[0], "caller", "subject"))
	assert.Equal(t, "req-1", groupField(t, got[1], "request", "requestId"))
	assert.Equal(t, "value", groupField(t, got[2], "data", "key"))
}

func TestEmit_ShouldWarnOnUnknownID(t *testing.T) { //nolint:paralleltest // mutates global hook
	withCleanRegistry(t)

	called := false

	prev := SetEmitterHook(func(id EventID, _ ...any) {
		if id == "TEST-404" {
			called = true
		}
	})

	t.Cleanup(func() { SetEmitterHook(prev) })

	require.NotPanics(t, func() {
		Emit(context.Background(), "TEST-404", "k", "v")
	})
	assert.True(t, called)
}

func TestEmit_ShouldBeSafeForConcurrentEmitters(t *testing.T) { //nolint:paralleltest // mutates global registry/hook
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-014", Level: slog.LevelInfo, MessageTemplate: "x"})

	// A hook exercises hookMu, which is process-global, on every emit.
	var count atomic.Int64

	prev := SetEmitterHook(func(id EventID, _ ...any) {
		if id == "TEST-014" {
			count.Add(1)
		}
	})

	t.Cleanup(func() { SetEmitterHook(prev) })

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			Emit(context.Background(), "TEST-014", "k", "v")
		}()
	}

	wg.Wait()

	assert.Equal(t, int64(goroutines), count.Load())
}
