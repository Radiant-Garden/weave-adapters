package etag

import (
	"bytes"
	"net/http"
)

// Conditional wraps a read handler with conditional-request support: it tags
// the response with a strong ETag and answers 304 Not Modified when the
// client's If-None-Match already matches.
//
// It is applied per handler rather than mounted in the server chain, because it
// buffers the response to hash it. That is the right trade for a JSON resource
// and the wrong one for a stream or a large download, so the choice stays with
// whoever writes the handler. Collections should be wrapped too — polling a
// list is exactly the case a 304 saves the most work on.
//
// Only GET and HEAD are handled. On other methods If-None-Match means something
// different (optimistic concurrency, M3's write side), and answering 304 there
// would be wrong.
func Conditional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)

			return
		}

		captured := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(captured, r)

		body := captured.body.Bytes()

		// Only a successful representation gets a tag. A 404 or a 502 has no
		// stable identity to cache, and tagging one would invite a client to
		// treat an error as a cacheable resource.
		if captured.status != http.StatusOK {
			w.WriteHeader(captured.status)
			_, _ = w.Write(body)

			return
		}

		tag := Compute(body)
		w.Header().Set("ETag", tag)

		if Matches(r.Header.Get("If-None-Match"), tag) {
			// net/http strips Content-Type and Content-Length for a 304 and
			// refuses body writes, so not writing is all that is required.
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.WriteHeader(captured.status)

		// The status is committed; a failed write is not actionable here, and
		// the request-completed event records what was sent.
		_, _ = w.Write(body)
	})
}

// captureWriter buffers a handler's response so it can be hashed before it is
// sent. Header mutations pass straight through to the real writer, so anything
// the handler set — Cache-Control, Content-Type — is already in place when the
// status is finally written.
type captureWriter struct {
	http.ResponseWriter

	status int
	body   bytes.Buffer
	wrote  bool
}

func (w *captureWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}

	w.wrote = true
	w.status = code
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.wrote = true

	return w.body.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController. Note that a
// handler cannot usefully flush through this wrapper — the response is buffered
// by design — which is the reason streaming handlers must not be wrapped.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
