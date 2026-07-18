package middleware

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// Recovery is the outermost middleware: it recovers a panic from any inner
// handler, emits API-011, and returns 500 — unless the response was already
// (partly) written, in which case it leaves it alone. http.ErrAbortHandler is
// re-panicked so net/http performs its silent connection abort. Because
// Recovery wraps RequestID, the caller context is not on its request when a
// panic unwinds, so it reads the request ID from the response header and passes
// request metadata as explicit data (API-011 is not an ExternalSource event).
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoveryWriter{ResponseWriter: w}

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec) // sentinel: let net/http abort the connection silently.
			}

			events.Emit(r.Context(), catalog.API011,
				"method", r.Method,
				"path", r.URL.Path,
				"remoteAddr", r.RemoteAddr,
				"requestId", rw.Header().Get(requestIDHeader),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)

			if rw.wrote {
				return // response already committed; a 500 now would corrupt it.
			}

			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusInternalServerError)
			// problem+json arrives with apierror in M2.
			_ = json.NewEncoder(rw).Encode(map[string]string{"error": "internal server error"})
		}()

		next.ServeHTTP(rw, r)
	})
}

// recoveryWriter tracks whether the response has started so Recovery does not
// write a 500 over an already-committed response. Unwrap keeps optional
// ResponseWriter capabilities reachable via http.ResponseController.
type recoveryWriter struct {
	http.ResponseWriter

	wrote bool
}

func (w *recoveryWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recoveryWriter) Write(b []byte) (int, error) {
	w.wrote = true

	return w.ResponseWriter.Write(b)
}

func (w *recoveryWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
