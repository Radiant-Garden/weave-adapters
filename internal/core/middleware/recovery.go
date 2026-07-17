package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// Recovery is the outermost middleware: it recovers a panic from any inner
// handler, emits API-011, and returns 500. Because it wraps RequestID, the
// caller context is not on its request when a panic unwinds, so it reads the
// request ID from the response header and passes request metadata as explicit
// data (API-011 is not an ExternalSource event).
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			events.Emit(r.Context(), catalog.API011,
				"method", r.Method,
				"path", r.URL.Path,
				"remoteAddr", r.RemoteAddr,
				"requestId", w.Header().Get(requestIDHeader),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			// If the handler already wrote a response, this encode is a no-op;
			// the panic is still recorded above. Body is a stable JSON shape
			// (problem+json arrives with apierror in M2).
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
		}()

		next.ServeHTTP(w, r)
	})
}
