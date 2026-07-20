// Package requestbody decodes a JSON request body under a size limit.
//
// It exists because every write handler needs the same four rejections before
// it can trust a body, and a handler that writes them itself will eventually
// write one of them differently. Decode returns an *apierror.Error for each, so
// the rejection reaches the client through apierror.WriteError — the one place
// in this codebase that logs and responds.
//
// Core, not adapter: nothing here knows what the body means. The limit arrives
// as a parameter rather than being read from config, so the package stays
// independent of how a binary is configured and the tests need no config at
// all.
package requestbody

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// mediaTypeJSON is the one media type the adapter accepts on a request body.
const mediaTypeJSON = "application/json"

// maxContentTypeLog bounds what a rejected Content-Type contributes to a log
// line. The header is attacker-controlled and unbounded; the diagnostic only
// needs enough of it to recognize what the client sent.
const maxContentTypeLog = 64

// Decode reads r's body as JSON into dst, rejecting anything that is not a
// well-formed JSON document of the expected shape, sent as application/json,
// within limit bytes.
//
// It returns an *apierror.Error on every rejection, so a handler can return the
// result unexamined. The four rejections and why each is separate:
//
//   - Not application/json — 415. Checked first, because it is the cheapest and
//     because a mislabelled body should not be read at all.
//   - Over limit — 413, via http.MaxBytesReader, which stops reading rather
//     than buffering and then measuring.
//   - Empty, malformed, or carrying unknown fields — 400.
//   - More than one JSON value — 400. A body of "{}{}" decodes its first value
//     successfully and would otherwise be accepted with the rest ignored.
//
// w is required by http.MaxBytesReader, which uses it to stop the connection
// being reused after an oversized body; it is not written to here.
func Decode(w http.ResponseWriter, r *http.Request, limit int, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(limit))

	decoder := json.NewDecoder(r.Body)

	// An unknown field is a rejection rather than something to ignore. A client
	// that sends a field this adapter does not know believes it set something;
	// silently dropping it means the caller and the server disagree about the
	// resource and neither can see it. That is the same silent-wrong-answer
	// failure the backend decoder rules out, on the inbound side.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return decodeError(err, limit)
	}

	// Decode stops at the end of the first JSON value. Anything after it was
	// sent but not honoured, so it is a rejection for the reason above.
	if decoder.More() {
		return apierror.Validation(apierror.FieldError{
			Field:   "body",
			Message: "must contain exactly one JSON object",
		})
	}

	return nil
}

// requireJSON rejects a body that is not labelled application/json.
//
// The header is the contract: a body that happens to be valid JSON under a
// different Content-Type is still rejected, because accepting it would make the
// header meaningless and leave the adapter guessing at a payload's type.
// Parameters are allowed and ignored, so "application/json; charset=utf-8"
// passes.
func requireJSON(r *http.Request) error {
	header := r.Header.Get("Content-Type")
	if header == "" {
		return apierror.New(catalog.API905, "contentType", "(none)")
	}

	// A Content-Type that will not parse is reported as what it was rather than
	// as absent — "(none)" would send an operator looking for a missing header
	// that the client did send.
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || mediaType != mediaTypeJSON {
		return apierror.New(catalog.API905, "contentType", truncate(header, maxContentTypeLog))
	}

	return nil
}

// decodeError maps a decode failure onto the response it deserves.
//
// Only the size limit gets its own code; every other failure is the client
// sending a body this adapter cannot read, which is a 400 whatever the
// underlying json error was.
func decodeError(err error, limit int) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return apierror.New(catalog.API904, "limitBytes", limit)
	}

	// An empty body reaches here as io.EOF. It is worth its own message: "must
	// be valid JSON" sends someone looking for a syntax error in a body that was
	// never sent.
	if errors.Is(err, io.EOF) {
		return apierror.Validation(apierror.FieldError{
			Field:   "body",
			Message: "must not be empty",
		})
	}

	// The json package's own message is returned to the client here, which is a
	// deliberate exception to the redaction rule and a narrow one: these
	// messages describe the caller's own bytes ("invalid character 'x' looking
	// for beginning of value", "unknown field \"foo\""), never adapter state.
	// Without it the client is told only that its body was wrong, which for a
	// mistyped field name is not enough to fix it.
	return apierror.Validation(apierror.FieldError{
		Field:   "body",
		Message: err.Error(),
	})
}

// truncate bounds an untrusted string for logging.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit] + "…"
}
