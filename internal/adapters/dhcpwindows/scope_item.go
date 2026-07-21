package dhcpwindows

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/requestbody"
)

// ScopeItemPath is the item route, mounted by the binary. The braces are a Go
// 1.22 ServeMux wildcard, and PathWadaptID reads the value back out.
const ScopeItemPath = "/api/v1/scopes/{wadaptId}"

// PathWadaptID names the path wildcard. A constant because the pattern and the
// r.PathValue call must agree, and a typo in either yields an empty string
// rather than an error — which would answer 404 for every request.
const PathWadaptID = "wadaptId"

// scopeDeleter and scopeUpdater are the write halves the item route serves, each
// declared where it is consumed. Splitting them from scopeLister is Go's idiom
// and also load-bearing: a handler holds exactly the methods its route uses, so
// the collection handler (which never deletes) and this one (which does) cannot
// reach past their own verbs by construction.
type scopeDeleter interface {
	DeleteScope(ctx context.Context, wadaptID string) error
}

type scopeUpdater interface {
	UpdateScope(ctx context.Context, wadaptID string, in ScopeUpdate) (Scope, error)
}

// scopeItemBackend is what the item handler needs: it reads on GET/HEAD, removes
// on DELETE and updates on PATCH.
type scopeItemBackend interface {
	scopeLister
	scopeDeleter
	scopeUpdater
}

// ScopeHandler serves GET, DELETE and PATCH on /api/v1/scopes/{wadaptId}.
//
// It holds exactly the three methods those verbs need. Earlier it took only the
// reader, when the route was read-only; the write side lands here rather than in
// a new handler because a create's Location header already points at this path,
// so the resource lives at one URL whether a client reads, updates or removes it.
type ScopeHandler struct {
	backend scopeItemBackend
	// maxBodyBytes bounds a PATCH body. It comes from core config rather than the
	// adapter's own, because the limit is a property of the server, not of DHCP.
	maxBodyBytes int
}

// NewScopeHandler returns the item handler, bounded by the core body limit for
// the PATCH it now serves.
func NewScopeHandler(backend scopeItemBackend, maxBodyBytes int) *ScopeHandler {
	return &ScopeHandler{backend: backend, maxBodyBytes: maxBodyBytes}
}

// ServeHTTP dispatches by method and delegates to apierror.WriteError, which is
// the one function that both logs and responds.
//
// The switch is exhaustive rather than default-to-GET: an earlier collection
// handler served its list on the default arm while its comment claimed the
// opposite, so a DELETE reached it as a 200. In the binary the mux mounts only
// GET, DELETE and PATCH, so any other method is a 405 from the router before it
// arrives here — the default arm matches that answer for a handler exercised
// without the mux instead of contradicting it.
func (h *ScopeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var err error

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		err = h.get(w, r)
	case http.MethodDelete:
		err = h.delete(w, r)
	case http.MethodPatch:
		err = h.update(w, r)
	default:
		// Unreachable in the binary: the mux mounts only GET, DELETE and PATCH on
		// this pattern, so the router answers 405 for anything else before it
		// arrives. This arm matches that answer for a handler mounted without the
		// mux, rather than contradicting it.
		err = apierror.New(catalog.API902, "method", r.Method, "allow", "GET, HEAD, DELETE, PATCH")
	}

	if err != nil {
		apierror.WriteError(w, r, err)
	}
}

// get returns the one scope whose wadaptID matches the path.
//
// It lists and scans, and that is not a shortcut to be optimized away later. A
// wadaptID is an HMAC of the server name and the subnet, so it cannot be
// reversed to the scopeId a targeted query would need — and Get-DhcpServerv4Scope
// has no "by our identity" form to call even if it could. One spawn, one scan;
// the same single backend call the collection makes.
//
// The scan is linear rather than a binary search over the sorted listing. The
// two are indistinguishable at DHCP scale, and a linear scan cannot go quietly
// wrong if ListScopes ever stops sorting — which the pagination cursor, not this
// handler, is what depends on.
func (h *ScopeHandler) get(w http.ResponseWriter, r *http.Request) error {
	wadaptID := r.PathValue(PathWadaptID)

	// No format validation on the way in. A wadaptID is opaque to the client by
	// design, so there is no shape to check that scanning would not settle
	// anyway, and rejecting a "malformed" one would leak how it is constructed.
	// Anything that matches nothing is a 404, whatever it looked like.
	scopes, err := h.backend.ListScopes(r.Context())
	if err != nil {
		return problemFor(err, opListScopes)
	}

	for i := range scopes {
		if scopes[i].WadaptID != wadaptID {
			continue
		}

		w.Header().Set("Content-Type", "application/json")

		// The ETag wrapper buffers this write and the status is committed
		// either way, so a write failure here is not actionable; API-010
		// records what was sent.
		_ = json.NewEncoder(w).Encode(scopes[i])

		return nil
	}

	// The identifier is echoed because it is the client's own input, and a 404
	// that does not say what was not found is unactionable when a client is
	// reconciling a set.
	//
	// Bounded on the way out. The path segment is attacker-controlled and limited
	// only by MaxHeaderBytes — around a megabyte — and it lands in both the 404
	// detail and API-900's resource field, so an unbounded echo is an amplifier
	// into log storage on a route that needs no credential to be wrong. A real
	// wadaptID is exactly WadaptIDLength characters, so the bound costs nothing a
	// legitimate client would notice.
	return apierror.NotFound("scope " + truncateWadaptID(wadaptID))
}

// delete removes the scope named by the path and answers 204.
//
// A wadaptID the server does not hold is a 404, not a 204: a reconciling client
// reads that as already-deleted, and answering 204 for a scope that never
// existed would claim a removal that did not happen. A backend fault keeps its
// own 502/504 rather than collapsing to a 404, which would tell that same client
// the scope was gone when the adapter simply could not reach the server.
func (h *ScopeHandler) delete(w http.ResponseWriter, r *http.Request) error {
	wadaptID := r.PathValue(PathWadaptID)

	if err := h.backend.DeleteScope(r.Context(), wadaptID); err != nil {
		if errors.Is(err, ErrScopeNotFound) {
			return apierror.NotFound("scope " + truncateWadaptID(wadaptID))
		}

		return problemFor(err, opDeleteScope)
	}

	w.WriteHeader(http.StatusNoContent)

	return nil
}

// update merge-updates the scope and answers 200 with its new representation.
//
// Validation runs before the backend is touched, the same order create keeps: a
// malformed body is the client's to fix, and spawning PowerShell to be told so
// wastes a second and puts a failed write in the DHCP server's own logs.
func (h *ScopeHandler) update(w http.ResponseWriter, r *http.Request) error {
	wadaptID := r.PathValue(PathWadaptID)

	var in ScopeUpdate
	if err := requestbody.Decode(w, r, h.maxBodyBytes, &in); err != nil {
		return err
	}

	if fieldErrors := in.Validate(); len(fieldErrors) > 0 {
		return apierror.Validation(fieldErrors...)
	}

	updated, err := h.backend.UpdateScope(r.Context(), wadaptID, in)
	if err != nil {
		return updateProblemFor(err, wadaptID)
	}

	w.Header().Set("Content-Type", "application/json")

	// Buffered by the ETag wrapper and the status is committed either way, so a
	// write failure here is not actionable; API-010 records what was sent.
	_ = json.NewEncoder(w).Encode(updated)

	return nil
}

// updateProblemFor maps an update failure, which has two outcomes list does not.
//
// A missing scope is a 404 — the answer weave treats as "target gone, re-create
// next cycle". A range that would leave the subnet is a validation failure
// naming the range field(s) that actually left it, because it is the client's
// input to fix and would have moved the scope's identity. Everything else is a
// backend code, distinct so the fault the adapter reports is the one weave reads.
func updateProblemFor(err error, wadaptID string) error {
	if errors.Is(err, ErrScopeNotFound) {
		return apierror.NotFound("scope " + truncateWadaptID(wadaptID))
	}

	var rangeErr *rangeOutsideSubnetError
	if errors.As(err, &rangeErr) {
		fieldErrors := make([]apierror.FieldError, 0, len(rangeErr.fields))
		for _, field := range rangeErr.fields {
			fieldErrors = append(fieldErrors,
				fieldError(field, "must keep the scope inside its existing subnet "+rangeErr.scopeID))
		}

		return apierror.Validation(fieldErrors...)
	}

	return problemFor(err, opUpdateScope)
}

// maxEchoedWadaptID bounds what an item 404 repeats back. Generous against
// WadaptIDLength on purpose: the point is to stop an amplifier, not to
// second-guess the format, and echoing a slightly-wrong ID in full is what makes
// the 404 actionable.
const maxEchoedWadaptID = 4 * WadaptIDLength

// truncateWadaptID bounds the echoed identifier. Cut by runes rather than bytes,
// so a multi-byte value cannot be split mid-sequence into the replacement
// character — the same rule apierror.TruncatePath and stderrContext follow.
func truncateWadaptID(id string) string {
	if utf8.RuneCountInString(id) <= maxEchoedWadaptID {
		return id
	}

	return string([]rune(id)[:maxEchoedWadaptID]) + "…"
}
