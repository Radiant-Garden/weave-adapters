package middleware

import (
	"net/http"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// Logging emits the API-010 request-completed event after the handler returns.
// It runs inside RequestID, so the request context carries the caller/request
// metadata the ExternalSource event needs. Request/response bodies are never
// logged.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		events.Emit(r.Context(), catalog.API010,
			"status", rec.status,
			"durationMs", time.Since(start).Milliseconds(),
			"bytesWritten", rec.bytes,
		)
	})
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
