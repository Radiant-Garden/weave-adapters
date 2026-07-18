package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// WriteError renders err as problem+json and emits its cataloged event, in that
// order of intent: one call is both the response and the log line, so the two
// cannot disagree. This is the single-handling rule — handlers return errors,
// they never log and respond.
//
// Any error that is not an *Error becomes a generic 500 with its message
// recorded in the event only, so an unmapped internal failure cannot leak a
// message that names internal hosts, queries, or file paths.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := asError(err)
	problem := apiErr.problem(r)

	// The API-9xx events are ExternalSource, and Emit panics on one with no
	// remoteAddr in context. A handler reached without the request-ID middleware
	// (a route mounted outside the chain, or a direct unit test) would otherwise
	// panic here and have its 404 turned into a 500 by the recovery middleware.
	ctx := events.EnsureCaller(r.Context(), events.Caller{
		RemoteAddr: r.RemoteAddr,
		Method:     r.Method,
		Path:       r.URL.Path,
	})

	// Emit before writing: if the client has gone away mid-write, the operator
	// still gets the record of what happened.
	events.Emit(ctx, apiErr.eventID, apiErr.eventData()...)

	WriteProblem(w, problem)
}

// WriteProblem writes a problem+json response without emitting an event. Use it
// only where the event was already emitted by the caller — the recovery
// middleware, which owns API-011 — so a panic yields one log line, not two.
func WriteProblem(w http.ResponseWriter, problem Problem) {
	status := problem.Status
	if status < http.StatusContinue {
		// net/http panics on a status below 100, so a Problem built without one
		// would crash the handler instead of answering. 500 is the honest
		// reading of "the caller did not say", and it keeps the response
		// serveable.
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)

	// The status is already committed, so a write failure here is not
	// actionable; the request-completed event records what was sent.
	_ = json.NewEncoder(w).Encode(problem)
}

// asError resolves any error to an *Error, mapping an unrecognized one onto the
// internal catch-all. errors.As is used rather than a type assertion so a
// wrapped *Error still resolves to its own taxonomy entry.
func asError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}

	return Internal(err)
}

// problem renders the client-facing body. Everything in it comes from the
// catalog entry or from fields the caller marked client-safe; the internal
// cause is deliberately absent.
func (e *Error) problem(r *http.Request) Problem {
	spec, ok := events.Get(e.eventID)
	if !ok {
		// An unregistered ID means the catalog and the constructors disagree.
		// Answer 500 rather than a half-built body naming an event nobody can
		// look up.
		return Problem{
			Type:      typePrefix + string(events.CodeInternal),
			Title:     taxonomy[events.CodeInternal].title,
			Status:    http.StatusInternalServerError,
			Detail:    "An unexpected error occurred.",
			Instance:  r.URL.Path,
			RequestID: events.CallerFrom(r.Context()).RequestID,
		}
	}

	meaning := lookup(spec.ResponseCode)

	return Problem{
		Type:         typePrefix + string(spec.ResponseCode),
		Title:        meaning.title,
		Status:       meaning.status,
		Detail:       render(spec.ResponseDetail, e.fields),
		Instance:     r.URL.Path,
		RequestID:    events.CallerFrom(r.Context()).RequestID,
		BackendError: e.backendError,
		Errors:       e.fieldErrors,
	}
}

// eventData renders the error's fields as Emit key/value pairs. Keys are sorted
// so a given error always logs its fields in the same order, which keeps log
// diffing and golden-output tests stable.
func (e *Error) eventData() []any {
	keys := make([]string, 0, len(e.fields))
	for k := range e.fields {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	data := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		data = append(data, k, e.fields[k])
	}

	return data
}

// render substitutes {{key}} placeholders in a template.
//
// An unresolved placeholder is left verbatim rather than blanked: "{{backend}}"
// in a response is an obvious bug report, whereas "The  backend could not be
// reached." reads like a cosmetic glitch and survives review.
func render(template string, fields map[string]any) string {
	if len(fields) == 0 || !strings.Contains(template, "{{") {
		return template
	}

	pairs := make([]string, 0, len(fields)*2)
	for key, value := range fields {
		pairs = append(pairs, "{{"+key+"}}", fmt.Sprint(value))
	}

	return strings.NewReplacer(pairs...).Replace(template)
}
