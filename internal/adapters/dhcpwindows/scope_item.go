package dhcpwindows

import (
	"encoding/json"
	"net/http"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// ScopeItemPath is the item route, mounted by the binary. The braces are a Go
// 1.22 ServeMux wildcard, and PathWadaptID reads the value back out.
const ScopeItemPath = "/api/v1/scopes/{wadaptId}"

// PathWadaptID names the path wildcard. A constant because the pattern and the
// r.PathValue call must agree, and a typo in either yields an empty string
// rather than an error — which would answer 404 for every request.
const PathWadaptID = "wadaptId"

// ScopeHandler serves GET /api/v1/scopes/{wadaptId}.
//
// It takes only the reader. The collection handler needs both halves because it
// serves POST; this one never writes, and holding the narrower interface is
// what makes that structural rather than a convention.
type ScopeHandler struct {
	backend scopeLister
}

// NewScopeHandler returns the item handler.
func NewScopeHandler(backend scopeLister) *ScopeHandler {
	return &ScopeHandler{backend: backend}
}

// ServeHTTP delegates to apierror.WriteError, which is the one function that
// both logs and responds.
func (h *ScopeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.get(w, r); err != nil {
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
	return apierror.NotFound("scope " + wadaptID)
}
