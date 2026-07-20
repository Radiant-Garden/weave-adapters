package dhcpwindows

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
	"github.com/radiantgarden/weave-adapters/internal/core/requestbody"
)

// ScopesPath is the collection route, mounted by the binary.
const ScopesPath = "/api/v1/scopes"

// ParamScopeID filters the collection to one subnet. A filter, not the resource
// key: scopeId is unique per server but private network addresses repeat across
// installations, so it does not identify a scope across a fleet. wadaptId does.
const ParamScopeID = "scopeId"

// paginationScope travels inside every cursor this collection mints, which is
// what makes a token from another collection unusable here. Changing it
// invalidates outstanding tokens.
const paginationScope = "dhcp-windows-scopes"

// scopeLister is the one method the handler needs from the backend.
//
// Declared here, at the consumer, rather than exported beside *Client: that is
// Go's idiom, and it is also what stops the interface growing to mirror the
// implementation as write support lands. A handler that only reads should not
// be able to reach a method that writes.
type scopeLister interface {
	ListScopes(ctx context.Context) ([]Scope, error)
}

// scopeCreator is the write half, declared separately so the read handlers
// cannot reach it. The collection handler embeds both because it serves both
// methods; ScopeHandler takes only the reader, which is what stops a future
// edit to the item route from growing a write.
type scopeCreator interface {
	CreateScope(ctx context.Context, in ScopeInput) (Scope, error)
}

// scopeBackend is what the collection handler needs: it lists on GET and
// creates on POST.
type scopeBackend interface {
	scopeLister
	scopeCreator
}

// ScopesHandler serves GET and POST /api/v1/scopes.
type ScopesHandler struct {
	backend scopeBackend
	pages   pagination.Paginator
	// maxBodyBytes bounds a create body. It comes from core config rather than
	// the adapter's own, because the limit is a property of the server and not
	// of DHCP.
	maxBodyBytes int
}

// NewScopesHandler returns the collection handler, paginated per the adapter's
// configured page sizes and bounded by the core body limit.
//
// It panics on an unusable page-size configuration, via pagination.New. Config
// validation rejects those values before this is reached in the binary, so a
// panic here means a handler built directly in a test.
func NewScopesHandler(backend scopeBackend, cfg Config, maxBodyBytes int) *ScopesHandler {
	return &ScopesHandler{
		backend:      backend,
		pages:        pagination.New(paginationScope, cfg.DefaultPageSize, cfg.MaxPageSize),
		maxBodyBytes: maxBodyBytes,
	}
}

// ServeHTTP is the single place this handler turns an error into a response,
// and it does so by delegating: apierror.WriteError is the one function that
// both logs and responds. Everything below returns an error instead.
//
// The method switch is here rather than in the mux because both methods live on
// one pattern and share the handler's state. It is exhaustive rather than
// default-to-list: an earlier version served the list on the default arm while
// its comment claimed the opposite, so a DELETE mounted straight onto this
// handler in a test was answered with a 200 list. In the binary the mux mounts
// only GET and POST, so any other method is a 405 from the router before it
// reaches here — the explicit arm below matches that answer for a handler
// exercised without the mux, instead of contradicting it.
func (h *ScopesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var err error

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		err = h.list(w, r)
	case http.MethodPost:
		err = h.create(w, r)
	default:
		// Unreachable in the binary: the mux mounts only GET and POST on this
		// pattern, so the router answers 405 for anything else before it arrives.
		// This arm exists for a handler mounted without the mux (a direct test),
		// and matches the router's answer rather than contradicting it. API-902 is
		// the middleware's in production; this is the same outcome from the one
		// place that can produce it when the middleware is absent.
		err = apierror.New(catalog.API902, "method", r.Method, "allow", "GET, HEAD, POST")
	}

	if err != nil {
		apierror.WriteError(w, r, err)
	}
}

// create decodes a scope, creates it, and answers 201 with the new resource.
//
// Validation runs before the backend is touched: a malformed input is the
// client's to fix, and spawning a PowerShell process to be told so wastes a
// second and puts a failed create in the DHCP server's own logs.
func (h *ScopesHandler) create(w http.ResponseWriter, r *http.Request) error {
	var in ScopeInput
	if err := requestbody.Decode(w, r, h.maxBodyBytes, &in); err != nil {
		return err
	}

	if fieldErrors := in.Validate(); len(fieldErrors) > 0 {
		return apierror.Validation(fieldErrors...)
	}

	scope, err := h.backend.CreateScope(r.Context(), in)
	if err != nil {
		return createProblemFor(err, in)
	}

	// Location must resolve, which is why the item route ships with this one.
	// A 201 pointing at a 404 is worse than no header: a client that follows it
	// concludes the create did not happen.
	w.Header().Set("Location", ScopesPath+"/"+scope.WadaptID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	_ = json.NewEncoder(w).Encode(scope)

	return nil
}

// createProblemFor maps a create failure, which has one outcome list does not.
//
// A conflict is a 409 rather than a backend code: the backend answered
// correctly and the answer was "that subnet is taken". It is also the one
// failure here a client can act on without an operator — the fix is to update
// the scope that is already there.
//
// The subnet is recomputed from the input rather than parsed out of the error
// string. Both name the same value — CreateScope derives it the same way — and
// deriving it is the option that cannot break when someone rewords the error.
func createProblemFor(err error, in ScopeInput) error {
	if errors.Is(err, ErrScopeExists) {
		// The subnet reaches the client, because "a scope already exists" with
		// no subnet named is unactionable when a client is reconciling several.
		// It is the client's own input echoed back, not internal state.
		//
		// The error is ignored: reaching a conflict means CreateScope already
		// derived this successfully, so it cannot fail here. An empty string
		// would render a detail naming no subnet, which is the pre-existing
		// behaviour rather than a new failure.
		scopeID, _ := in.ScopeID()

		return apierror.New(adapterevents.BACKEND105, "scopeId", scopeID).WithCause(err)
	}

	return problemFor(err, opCreateScope)
}

// list resolves the query, reads the backend, and writes one page.
func (h *ScopesHandler) list(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()

	params, filter, err := h.parseQuery(query)
	if err != nil {
		return err
	}

	scopes, err := h.backend.ListScopes(r.Context())
	if err != nil {
		return problemFor(err, opListScopes)
	}

	// Filter before paging, so pageSize counts matching scopes rather than
	// scanned ones. Filtering after would return short pages — or an empty one
	// with a next cursor — for a filter that excludes most of the collection.
	scopes = filterByScopeID(scopes, filter)

	page, next := h.paginate(scopes, params, r)

	w.Header().Set("Content-Type", "application/json")

	// The ETag wrapper buffers this write, and the status is committed either
	// way, so a write failure here is not actionable; API-010 records what was
	// sent.
	_ = json.NewEncoder(w).Encode(pagination.NewPage(page, next))

	return nil
}

// filterByScopeID returns the scopes matching filter, or all of them when no
// filter was given.
//
// It builds a new slice rather than compacting in place, and that is the whole
// point of the function existing. slices.DeleteFunc would filter the caller's
// slice: it moves matches to the front, zeroes the tail, and returns a shorter
// header over the *same* array — so the listing the backend handed us comes
// back with its remaining entries blanked. Today Client.ListScopes allocates
// fresh on every call, so nothing shares that array and the damage is invisible;
// the moment anything caches a listing (which the cache phase is specified to
// do — it holds the last read) one filtered request would poison it, and every
// later request would serve scopes with an empty scopeId and an empty wadaptId.
//
// A scope with no identity is exactly what this milestone's central invariant
// forbids, and an empty scopeId is what decodeScopes rejects on the way in. It
// is not worth leaving a landmine that produces one, to save an allocation on a
// path that has just spawned a PowerShell process.
func filterByScopeID(scopes []Scope, filter string) []Scope {
	if filter == "" {
		return scopes
	}

	// At most one match: Windows permits exactly one scope per subnet, and
	// scopeId *is* the subnet.
	matched := make([]Scope, 0, 1)

	for _, s := range scopes {
		if s.ScopeID == filter {
			matched = append(matched, s)
		}
	}

	return matched
}

// parseQuery resolves both query concerns, reporting every failure at once.
//
// Both are validated before either is rejected, because this API's rule is that
// a client fixes all its mistakes in one round trip rather than discovering
// them one attempt at a time. Returning pagination's error the moment it
// appeared would hide a bad scopeId behind a bad pageSize.
func (h *ScopesHandler) parseQuery(query url.Values) (pagination.Params, string, error) {
	var fieldErrors []apierror.FieldError

	params, pageErr := h.pages.Parse(query)

	var apiErr *apierror.Error
	if errors.As(pageErr, &apiErr) {
		fieldErrors = append(fieldErrors, apiErr.FieldErrors()...)
	} else if pageErr != nil {
		// Not a validation error at all, so it is not this function's to
		// summarize.
		return pagination.Params{}, "", pageErr
	}

	filter, filterErrs := parseScopeIDFilter(query)
	fieldErrors = append(fieldErrors, filterErrs...)

	if len(fieldErrors) > 0 {
		return pagination.Params{}, "", apierror.Validation(fieldErrors...)
	}

	return params, filter, nil
}

// parseScopeIDFilter reads ?scopeId=, rejecting a value that is not an IPv4
// address.
//
// Rejected rather than answered with an empty page, for the reason
// pagination rejects a non-integer pageSize: there is no honest reading of
// "notanaddress" as a filter. Every scopeId in this collection is an IPv4
// address — the client validates that on decode — so a malformed one can never
// match, and answering 200 with an empty list would tell a client its filter
// worked and the server has no such scope.
func parseScopeIDFilter(query url.Values) (string, []apierror.FieldError) {
	// Get takes the first value of a repeated parameter, which is what net/http
	// does everywhere else.
	raw := query.Get(ParamScopeID)
	if raw == "" {
		return "", nil
	}

	addr, err := netip.ParseAddr(raw)
	if err != nil || !addr.Is4() {
		return "", []apierror.FieldError{{
			Field:   ParamScopeID,
			Message: "must be an IPv4 address, e.g. 192.168.178.0",
		}}
	}

	// The parsed form, so the comparison can only ever see a spelling the
	// backend could also produce.
	//
	// netip is stricter than it looks, and deliberately so: it rejects leading
	// zeros ("010.0.0.0"), because they read as octal in some resolvers and as
	// decimal in others — an ambiguity that has produced real SSRF and
	// access-control bypasses. Such a value is a 400 here rather than being
	// guessed at, which is the right end of that trade for a filter.
	return addr.String(), nil
}

// paginate returns the page starting after the cursor, and the cursor for the
// next one.
//
// The listing arrives sorted by wadaptID from ListScopes, and the resume
// compares the same encoded string. One comparator in both places is what makes
// the cursor safe: IPv4 dotted strings do not order the way addresses do
// ("192.168.178.0" sorts before "192.168.2.0" as text, after as an address), so
// sorting on one form and resuming on another would skip and repeat pages in
// silence.
// Both of pagination.Slice's preconditions are met here rather than assumed:
// ListScopes sorts by the encoded wadaptID and rejects a duplicate derivation,
// so the resume key is both the sort key and unique. That rejection is not
// defensive tidying — a repeated key is a repeated cursor, and the walk would
// silently drop the rest of the run at a page boundary.
func (h *ScopesHandler) paginate(
	scopes []Scope, params pagination.Params, r *http.Request,
) ([]Scope, pagination.NextPage) {
	return pagination.Slice(h.pages, scopes, params, r.URL, func(s Scope) string {
		return s.WadaptID
	})
}

// problemFor maps a backend failure onto the response taxonomy.
//
// The three backend outcomes stay distinct all the way to the status code
// because that is the only part weave reads — its classifier is given the
// status and never the body. "Unreachable" (502) and "answered with nonsense"
// (502, different type) and "too slow" (504) want different operator responses,
// and collapsing them into one generic code would put an adapter bug and a
// dead DHCP server behind the same signal.
//
// A duplicate wadaptID is deliberately *not* a backend code. The backend
// answered correctly; two scopes collided under this adapter's own derivation,
// which is our fault and a 500.
//
// No WithBackendError. The extension exists for a sanitized backend message,
// and these carry raw PowerShell stderr, which can name internal hosts and
// paths. It reaches the operator through WithCause and through BACKEND-101,
// which is where it belongs; the client gets the curated ResponseDetail.
// operation names which backend call failed, so the diagnostic says whether a
// list or a create was in flight.
func problemFor(err error, operation string) error {
	switch {
	case errors.Is(err, ErrBackendTimeout):
		return apierror.New(adapterevents.BACKEND103, "operation", operation).WithCause(err)

	case errors.Is(err, ErrBackendUnavailable):
		return apierror.New(adapterevents.BACKEND102, "operation", operation).WithCause(err)

	case errors.Is(err, ErrBackendMalformed):
		return apierror.New(adapterevents.BACKEND104, "operation", operation).WithCause(err)

	default:
		// ErrDuplicateWadaptID lands here, as does anything ListScopes grows
		// later. A new typed error surfacing as a 500 until it is mapped is the
		// safe direction: it is loud, it does not blame the backend, and
		// apierror.Internal keeps the cause out of the response.
		return apierror.Internal(err)
	}
}
