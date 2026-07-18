package events

import "context"

// callerContextKey is a private type for caller-context keys, preventing
// collisions with keys defined by other packages.
type callerContextKey string

// ctxCaller holds a *Caller rather than the fields individually, so an inner
// middleware can enrich what an outer one later reads. Authentication runs
// inside request logging but must appear on its audit line: with immutable
// values, auth's context would be visible only to handlers beneath it, and the
// API-010 line — the request audit record — would carry an empty subject.
const ctxCaller callerContextKey = "caller"

// Caller carries the identity and request metadata attached to ExternalSource
// events. Middleware populates it from the inbound request; Emit reads it back.
type Caller struct {
	Subject    string
	Role       string
	RemoteAddr string
	RequestID  string
	Method     string
	Path       string
}

// WithCaller returns a context carrying the caller identity and request
// metadata. The request-ID and auth middleware call this so ExternalSource
// events can attach the caller/request groups.
func WithCaller(ctx context.Context, c Caller) context.Context {
	stored := c

	return context.WithValue(ctx, ctxCaller, &stored)
}

// SetIdentity records the authenticated caller on the context established by
// WithCaller, reporting whether there was one to record onto.
//
// It mutates in place so middleware that already ran — request logging, which
// wraps authentication — observes the identity when it emits. The write happens
// before the handler runs and nothing writes afterwards, so the later reads are
// ordered after it without further synchronization.
func SetIdentity(ctx context.Context, subject, role string) bool {
	caller, ok := ctx.Value(ctxCaller).(*Caller)
	if !ok || caller == nil {
		return false
	}

	caller.Subject = subject
	caller.Role = role

	return true
}

// CallerFrom reads the caller metadata attached by WithCaller, returning zero
// values if absent. Middleware uses it to check whether a request already
// carries caller context.
func CallerFrom(ctx context.Context) Caller {
	return callerFrom(ctx)
}

// EnsureCaller returns a context guaranteed to satisfy the ExternalSource
// contract: if ctx already carries a remoteAddr it is returned untouched,
// otherwise fallback is attached.
//
// Emit panics on an ExternalSource event with no remoteAddr, which is the right
// guard for a background context but the wrong outcome for a handler on a
// request that simply never passed through the request-ID middleware — a route
// mounted outside the chain, or a handler under direct unit test. Every emitter
// on the request path calls this so a missing caller degrades to a
// request-derived one instead of a panic.
func EnsureCaller(ctx context.Context, fallback Caller) context.Context {
	if callerFrom(ctx).RemoteAddr != "" {
		return ctx
	}

	return WithCaller(ctx, fallback)
}

// callerFrom reads the caller metadata from the context (zero values if
// absent). It returns a copy, so a reader cannot alter what later events see.
func callerFrom(ctx context.Context) Caller {
	caller, ok := ctx.Value(ctxCaller).(*Caller)
	if !ok || caller == nil {
		return Caller{}
	}

	return *caller
}

// InternalActorCtx returns a context satisfying the ExternalSource contract for
// system-driven events (background sweeps, restart recovery) that still need
// the external shape. role is "system" and remoteAddr is "internal", so SIEM
// filters can split system-driven events (caller.role=system) from real callers.
func InternalActorCtx(actor, requestID string) context.Context {
	return WithCaller(context.Background(), Caller{
		Subject:    actor,
		Role:       "system",
		RemoteAddr: "internal",
		RequestID:  requestID,
	})
}
