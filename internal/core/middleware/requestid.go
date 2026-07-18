package middleware

import (
	"crypto/rand"
	"fmt"
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// RequestID accepts an inbound X-Request-Id or generates one, echoes it in the
// response header, and populates the caller/request context the events system
// reads. It runs before Logging so the request-completed event's ExternalSource
// guard (remoteAddr present) is satisfied. Auth later fills in subject/role.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(requestIDHeader, id)

		ctx := events.WithCaller(r.Context(), events.Caller{
			RequestID:  id,
			Method:     r.Method,
			Path:       r.URL.Path,
			RemoteAddr: r.RemoteAddr,
		})

		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
