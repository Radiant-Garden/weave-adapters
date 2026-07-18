package middleware

import (
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/httpx"
)

// Logging returns middleware that emits the API-010 request-completed event
// after each request. Requests for which skip returns true (e.g. high-frequency
// health polls) are served but not logged. skip receives the response status so
// a filter can suppress routine traffic without also hiding failures on the
// same path. It runs inside RequestID; if the
// caller context is absent it seeds remoteAddr from the request so the
// ExternalSource event never panics. Request/response bodies are never logged.
func Logging(skip func(r *http.Request, status int) bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := httpx.NewRecorder(w)

			next.ServeHTTP(rec, r)

			if skip != nil && skip(r, rec.Status()) {
				return
			}

			ctx := events.EnsureCaller(r.Context(), events.Caller{
				RemoteAddr: r.RemoteAddr,
				Method:     r.Method,
				Path:       r.URL.Path,
			})

			events.Emit(ctx, catalog.API010,
				"status", rec.Status(),
				"durationMs", time.Since(start).Milliseconds(),
				"bytesWritten", rec.Bytes(),
			)
		})
	}
}
