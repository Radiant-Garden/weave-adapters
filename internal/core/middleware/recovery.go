package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/httpx"
)

// Recovery is the outermost middleware: it recovers a panic from any inner
// handler, emits API-011, and returns a 500 problem+json — unless the response
// was already (partly) written, in which case it leaves it alone.
// http.ErrAbortHandler is
// re-panicked so net/http performs its silent connection abort. Because
// Recovery wraps RequestID, the caller context is not on its request when a
// panic unwinds, so it reads the request ID from the response header and passes
// request metadata as explicit data (API-011 is not an ExternalSource event).
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := httpx.NewRecorder(w)

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
				"path", apierror.TruncatePath(r.URL.Path),
				"remoteAddr", r.RemoteAddr,
				"requestId", rw.Header().Get(requestIDHeader),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)

			if rw.Wrote() {
				return // response already committed; a 500 now would corrupt it.
			}

			// WriteProblem, not WriteError: API-011 above is already this
			// panic's log line, and WriteError would emit API-901 as well,
			// making one panic look like two failures.
			apierror.WriteProblem(rw, apierror.Problem{
				Type:      apierror.TypeFor(events.CodeInternal),
				Title:     apierror.TitleFor(events.CodeInternal),
				Status:    http.StatusInternalServerError,
				Detail:    "An unexpected error occurred.",
				Instance:  apierror.TruncatePath(r.URL.Path),
				RequestID: rw.Header().Get(requestIDHeader),
			})
		}()

		next.ServeHTTP(rw, r)
	})
}
