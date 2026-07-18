package middleware

import (
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// Logging returns middleware that emits the API-010 request-completed event
// after each request. Requests for which skip returns true (e.g. high-frequency
// health polls) are served but not logged. It runs inside RequestID; if the
// caller context is absent it seeds remoteAddr from the request so the
// ExternalSource event never panics. Request/response bodies are never logged.
func Logging(skip func(*http.Request) bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			if skip != nil && skip(r) {
				return
			}

			ctx := events.EnsureCaller(r.Context(), events.Caller{
				RemoteAddr: r.RemoteAddr,
				Method:     r.Method,
				Path:       r.URL.Path,
			})

			events.Emit(ctx, catalog.API010,
				"status", rec.status,
				"durationMs", time.Since(start).Milliseconds(),
				"bytesWritten", rec.bytes,
			)
		})
	}
}

// statusRecorder wraps an http.ResponseWriter to capture the status code and
// the number of body bytes written.
type statusRecorder struct {
	http.ResponseWriter

	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n

	return n, err
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController can
// reach optional capabilities (Flusher, Hijacker) that this wrapper hides —
// needed by streaming handlers such as a future SSE event stream.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
