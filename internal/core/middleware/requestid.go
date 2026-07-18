package middleware

import (
	"crypto/rand"
	"fmt"
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// RequestID accepts an inbound X-Request-Id or generates one, echoes it in the
// response header, and populates the caller/request context the events system
// reads. It runs before Logging so the request-completed event's ExternalSource
// guard (remoteAddr present) is satisfied. Auth later fills in subject/role.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !usableRequestID(id) {
			id = newRequestID()
		}

		w.Header().Set(requestIDHeader, id)

		// The path is bounded here because this is where it enters the caller
		// context, and from there every event line this request emits. Bounding
		// it once at the entry point beats truncating at each echo site and
		// missing one.
		ctx := events.WithCaller(r.Context(), events.Caller{
			RequestID:  id,
			Method:     r.Method,
			Path:       apierror.TruncatePath(r.URL.Path),
			RemoteAddr: r.RemoteAddr,
		})

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// maxInboundRequestIDLen bounds an accepted inbound correlation ID. Comfortably
// past a UUID or a trace ID, and far short of what makes an amplifier.
const maxInboundRequestIDLen = 128

// usableRequestID reports whether an inbound X-Request-Id can be adopted as this
// request's correlation key.
//
// The value is attacker-controlled and gets echoed into the response header, the
// problem body, and every event line, so an unbounded one is the same
// amplification the reflected path is bounded against. It is rejected rather
// than truncated: a truncated ID no longer correlates with the caller's own
// logs, which is worse than an obviously-fresh one they can see is not theirs.
func usableRequestID(id string) bool {
	if id == "" || len(id) > maxInboundRequestIDLen {
		return false
	}

	// Printable ASCII only. A correlation key ends up in log storage and in a
	// response header; control bytes there are somebody's problem downstream.
	for i := range len(id) {
		if c := id[i]; c < 0x20 || c > 0x7E {
			return false
		}
	}

	return true
}

// newRequestID returns a random UUIDv4-formatted string.
func newRequestID() string {
	var b [16]byte

	// Since Go 1.24 crypto/rand.Read never returns an error; it panics if the
	// system source fails. There is no fallback to write here — a shared
	// placeholder ID would silently merge unrelated requests' traces.
	_, _ = rand.Read(b[:])

	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
