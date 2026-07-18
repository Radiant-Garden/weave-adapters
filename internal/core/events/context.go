package events

import "context"

// callerContextKey is a private type for caller-context keys, preventing
// collisions with keys defined by other packages.
type callerContextKey string

const (
	ctxCallerSubject    callerContextKey = "caller_subject"
	ctxCallerRole       callerContextKey = "caller_role"
	ctxCallerRemoteAddr callerContextKey = "caller_remote_addr"
	ctxCallerRequestID  callerContextKey = "caller_request_id"
	ctxCallerMethod     callerContextKey = "caller_method"
	ctxCallerPath       callerContextKey = "caller_path"
)

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
	ctx = context.WithValue(ctx, ctxCallerSubject, c.Subject)
	ctx = context.WithValue(ctx, ctxCallerRole, c.Role)
	ctx = context.WithValue(ctx, ctxCallerRemoteAddr, c.RemoteAddr)
	ctx = context.WithValue(ctx, ctxCallerRequestID, c.RequestID)
	ctx = context.WithValue(ctx, ctxCallerMethod, c.Method)
	ctx = context.WithValue(ctx, ctxCallerPath, c.Path)

	return ctx
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

// callerFrom reads the caller metadata from the context (zero values if absent).
func callerFrom(ctx context.Context) Caller {
	str := func(k callerContextKey) string {
		v, _ := ctx.Value(k).(string)

		return v
	}

	return Caller{
		Subject:    str(ctxCallerSubject),
		Role:       str(ctxCallerRole),
		RemoteAddr: str(ctxCallerRemoteAddr),
		RequestID:  str(ctxCallerRequestID),
		Method:     str(ctxCallerMethod),
		Path:       str(ctxCallerPath),
	}
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
