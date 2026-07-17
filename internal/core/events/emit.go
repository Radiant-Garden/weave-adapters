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
	emit(ctx, id, "", dataKvs...)
}

// EmitWithMessage is Emit with a dynamic message overriding the template.
func EmitWithMessage(ctx context.Context, id EventID, message string, dataKvs ...any) {
	emit(ctx, id, message, dataKvs...)
}

func emit(ctx context.Context, id EventID, message string, dataKvs ...any) {
	hookMu.RLock()

	hook := emitterHook

	hookMu.RUnlock()

	event, ok := Get(id)
	if !ok {
		msg := message
		if msg == "" {
			msg = fmt.Sprintf("unknown event: %s", id)
		}

		if hook != nil {
			hook(id, dataKvs...)
		}

		slog.Warn(msg, append([]any{"eventId", string(id)}, dataKvs...)...)

		return
	}

	msg := message
	if msg == "" {
		msg = event.MessageTemplate
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

	fanOutToSubscribers(id, dataKvs)

	slog.Default().Log(ctx, event.Level, msg, attrs...)
}
