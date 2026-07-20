package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// emitterHook lets the testing package intercept emissions. It is nil in
// production.
var (
	hookMu      sync.RWMutex
	emitterHook func(id EventID, attrs ...any)
)

// SetEmitterHook installs a hook invoked on every emission and returns the
// previous hook. Used by the testing package to capture events.
func SetEmitterHook(hook func(id EventID, attrs ...any)) func(id EventID, attrs ...any) {
	hookMu.Lock()
	defer hookMu.Unlock()

	prev := emitterHook
	emitterHook = hook

	return prev
}

// Emit logs the event with the given ID and optional data key/value pairs.
//
// Internal events wrap dataKvs in a "data" group. ExternalSource events also
// attach "caller" and "request" groups read from ctx, and Emit panics if ctx
// carries no remoteAddr — guarding against a background context on a
// request-scoped event. An unregistered ID logs a loud warning instead of
// silently dropping.
func Emit(ctx context.Context, id EventID, dataKvs ...any) {
	hookMu.RLock()

	hook := emitterHook

	hookMu.RUnlock()

	event, ok := Get(id)
	if !ok {
		// The hook sees the data wrapped in the same "data" group it gets for a
		// registered event, so a recorder reading .Data(key) finds the fields on
		// an unregistered emission too rather than an empty map. The two paths
		// once handed the hook different shapes — raw pairs here, a group below —
		// and a test asserting on fields silently saw nothing for the unregistered
		// case.
		if hook != nil {
			hook(id, slog.Group("data", dataKvs...))
		}

		slog.Warn(fmt.Sprintf("unknown event: %s", id),
			append([]any{"eventId", string(id)}, dataKvs...)...)

		return
	}

	dataGroup := slog.Group("data", dataKvs...)
	attrs := []any{"eventId", string(id)}

	var hookAttrs []any

	if event.ExternalSource {
		c := callerFrom(ctx)
		if c.RemoteAddr == "" {
			panic(fmt.Sprintf(
				"event %s is ExternalSource but ctx has no remoteAddr; pass the request context instead of a background one",
				id,
			))
		}

		callerGroup := slog.Group("caller", "subject", c.Subject, "role", c.Role, "remoteAddr", c.RemoteAddr)
		requestGroup := slog.Group("request", "requestId", c.RequestID, "method", c.Method, "path", c.Path)

		attrs = append(attrs, callerGroup, requestGroup, dataGroup)
		hookAttrs = []any{callerGroup, requestGroup, dataGroup}
	} else {
		attrs = append(attrs, dataGroup)
		hookAttrs = []any{dataGroup}
	}

	if hook != nil {
		hook(id, hookAttrs...)
	}

	slog.Default().Log(ctx, event.Level, event.MessageTemplate, attrs...)
}
