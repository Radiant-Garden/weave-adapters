package etag

import (
	"bytes"
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// MaxTaggedBytes bounds how much of a response the wrapper will hold in memory
// to hash it. A response that grows past this is streamed through untagged
// rather than buffered without limit — an unpaginated collection would
// otherwise be held whole, once per in-flight request.
const MaxTaggedBytes = 4 << 20 // 4 MiB

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
//
// Two consequences of buffering are worth knowing before wrapping a handler:
//
//   - A response over MaxTaggedBytes is sent untagged and emits API-012. The
//     route still works; it just stops being cheap to poll.
//   - Handler writes land in a buffer and always succeed, so a handler cannot
//     learn from a failed Write that the client has gone away. Use the request
//     context for cancellation, which is unaffected.
func Conditional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)

			return
		}

		captured := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(captured, r)

		// Too large to tag: the response has already been streamed through, so
		// there is nothing left to write — only to report.
		if captured.overflowed {
			ctx := events.EnsureCaller(r.Context(), events.Caller{
				RemoteAddr: r.RemoteAddr,
				Method:     r.Method,
				Path:       r.URL.Path,
			})

			events.Emit(ctx, catalog.API012, "path", r.URL.Path, "limitBytes", MaxTaggedBytes)

			return
		}

		// The client gave up while the handler was working. Hashing and writing
		// a response nobody will read is pure waste, and the write would fail
		// anyway.
		if r.Context().Err() != nil {
			return
		}

		body := captured.body.Bytes()

		// Only a 200 gets a tag — exactly 200, not any 2xx. A 404 or a 502 has
		// no stable identity to cache, and tagging one would invite a client to
		// treat an error as a cacheable resource. A 203 or 206 is excluded for a
		// duller reason: nothing here produces one, and a 206 would need
		// Content-Range in the tag's identity to be correct. Widening this
		// without that is how a partial response gets cached as a whole one.
		if captured.status != http.StatusOK {
			// Strip any tag the inner handler set. "Errors pass through
			// untagged" is the documented contract, and this wrapper cannot
			// vouch for a tag it did not compute.
			w.Header().Del("ETag")
			w.WriteHeader(captured.status)
			_, _ = w.Write(body)

			return
		}

		// Set, not Add: this wrapper's tag is the authoritative one, since it is
		// the only tag computed from the bytes actually being sent. An inner
		// handler that set its own — a backend version field, which the package
		// doc rules out — is overwritten here rather than allowed to disagree
		// with the body.
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
//
// It deliberately does not share the recorder used by the middleware wrappers:
// those forward every write onward, and this one withholds writes until the
// body is complete. That inversion is the whole point of the type, so composing
// it from a forwarding recorder would obscure rather than share.
type captureWriter struct {
	http.ResponseWriter

	status int
	body   bytes.Buffer
	wrote  bool
	// overflowed records that the body outgrew MaxTaggedBytes, at which point
	// the buffered bytes were flushed and the response became a pass-through.
	overflowed bool
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

	if w.overflowed {
		return w.ResponseWriter.Write(b)
	}

	if w.body.Len()+len(b) <= MaxTaggedBytes {
		return w.body.Write(b)
	}

	// Past the limit: commit the status, flush what was buffered, and let this
	// and every later write go straight through. The response loses its ETag
	// but is otherwise served normally — degrading to "not cacheable" beats
	// holding an unbounded body in memory.
	w.overflowed = true

	w.ResponseWriter.WriteHeader(w.status)

	if _, err := w.ResponseWriter.Write(w.body.Bytes()); err != nil {
		return 0, err
	}

	w.body.Reset()

	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController, so
// capabilities this wrapper does not intercept — SetWriteDeadline, and Hijack
// for a handler that means to take the connection over entirely — stay
// reachable.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// FlushError refuses to flush rather than letting http.ResponseController reach
// the real writer through Unwrap. A flush there would commit a 200 and the
// headers on the wire while the body still sat in this buffer: the ETag set
// afterwards would be dropped, and the 304 branch would return a committed 200
// with no body at all. Refusing is what tells a streaming handler it is wrapped
// in something it must not be wrapped in — silently doing nothing would leave
// it believing the flush had happened.
func (w *captureWriter) FlushError() error {
	return http.ErrNotSupported
}
