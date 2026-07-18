// Package httpx holds the small HTTP plumbing shared by the core middleware.
package httpx

import "net/http"

// Recorder wraps an http.ResponseWriter and records what was sent: the status,
// the number of body bytes, and whether the response has been committed at all.
//
// It exists because four wrappers in this codebase grew the same bookkeeping
// independently and got it subtly different — one ignored a second WriteHeader,
// another let a Write commit the response without a status. Sharing the common
// part means a fix lands once. Wrappers that genuinely differ (one that
// withholds writes to buffer them, one that replaces a response mid-flight)
// still implement their own, and say why.
type Recorder struct {
	http.ResponseWriter

	status int
	bytes  int
	wrote  bool
}

// NewRecorder wraps w. The status defaults to 200, matching net/http: a handler
// that writes a body without calling WriteHeader has sent a 200.
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records and forwards the status. The first call wins, as it does
// in net/http, so a late second call cannot rewrite history.
func (r *Recorder) WriteHeader(code int) {
	if r.wrote {
		return
	}

	r.wrote = true
	r.status = code

	r.ResponseWriter.WriteHeader(code)
}

// Write forwards the body and accumulates the byte count.
func (r *Recorder) Write(b []byte) (int, error) {
	r.wrote = true

	n, err := r.ResponseWriter.Write(b)
	r.bytes += n

	return n, err
}

// Status returns the status sent, or 200 if the handler wrote a body without
// setting one.
func (r *Recorder) Status() int { return r.status }

// Bytes returns the number of body bytes written.
func (r *Recorder) Bytes() int { return r.bytes }

// Wrote reports whether the response has been committed — either a status or a
// body byte has gone out, so it is too late to replace it.
func (r *Recorder) Wrote() bool { return r.wrote }

// Unwrap exposes the underlying writer so http.ResponseController can reach
// optional capabilities (Flusher, Hijacker) that this wrapper hides.
func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
