// Package httptest provides the demo resource that proves M2's HTTP surface
// composes. Import it with an alias, e.g. coretest
// "github.com/radiantgarden/weave-adapters/internal/core/httptest".
//
// It is test-only and never mounted in the shipped binary: core packages must
// stay adapter-agnostic, and a demo collection in a DHCP adapter would be a
// route weave could call. TestDemo_ShouldNotBeReachableFromTheBinary enforces
// that rather than trusting review.
//
// The resource exists because every other M2 package can be right on its own
// and still not compose — auth populating a caller the logging middleware never
// sees, an ETag computed over a body pagination later changes. This is the one
// place where auth, conditional reads, pagination and problem+json run through
// the real middleware chain together.
package httptest

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/etag"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
	"github.com/radiantgarden/weave-adapters/internal/core/middleware"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

// Route paths the demo resource serves.
const (
	CollectionPath = "/api/v1/items"
	ItemPath       = CollectionPath + "/{id}"
)

// Page sizes for the demo collection. Small enough that a handful of items
// spans several pages, so a test can walk them without building a large fixture.
const (
	DefaultPageSize = 2
	MaxPageSize     = 10
)

// Item is the demo resource's representation.
type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Resource is an in-memory collection served with the full M2 treatment:
// paginated list, conditional reads, and problem+json errors.
type Resource struct {
	// items is kept sorted by ID, because the page cursor resumes after a key
	// and a listing whose order does not match the cursor would skip or repeat
	// rows across pages.
	items []Item
	pages pagination.Paginator
}

// NewResource returns a demo resource holding items, which it sorts by ID.
//
// It panics if any ID is empty or repeated. A keyset cursor resumes strictly
// after a key, so the key has to be unique and non-empty or the listing loses
// rows without erroring:
//
//   - A repeated ID makes a page that ends mid-run skip the rest of that run.
//   - An empty ID mints no cursor, so a page ending on one reports itself as
//     the last page while items remain.
//
// Both are silent — the walk simply returns fewer rows than exist. Adapters
// building a real listing inherit this constraint, so it fails loudly here
// rather than being a property this fixture happens to have.
func NewResource(items ...Item) *Resource {
	seen := make(map[string]bool, len(items))

	for _, item := range items {
		if item.ID == "" {
			panic("httptest: item ID must not be empty; a cursor cannot resume after it")
		}

		if seen[item.ID] {
			panic("httptest: duplicate item ID " + item.ID + "; a cursor cannot resume after a repeated key")
		}

		seen[item.ID] = true
	}

	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b Item) int { return cmp.Compare(a.ID, b.ID) })

	return &Resource{
		items: sorted,
		pages: pagination.New("demo-items", DefaultPageSize, MaxPageSize),
	}
}

// Mount registers the resource's routes. Both are wrapped in etag.Conditional:
// they produce whole JSON documents, which is what that wrapper is for.
func (r *Resource) Mount(mux *http.ServeMux) {
	mux.Handle("GET "+CollectionPath, etag.Conditional(http.HandlerFunc(r.list)))
	mux.Handle("GET "+ItemPath, etag.Conditional(http.HandlerFunc(r.get)))
}

// Handler returns the resource mounted behind the adapter's standard chain,
// with inner applied where authentication goes.
//
// It calls httpserver.NewHandler rather than assembling the chain here, so what
// the tests exercise is the chain the server actually runs.
func (r *Resource) Handler(inner ...middleware.Middleware) http.Handler {
	mux := http.NewServeMux()
	r.Mount(mux)

	return httpserver.NewHandler(mux, inner...)
}

// list serves the paginated collection.
func (r *Resource) list(w http.ResponseWriter, req *http.Request) {
	params, err := r.pages.Parse(req.URL.Query())
	if err != nil {
		apierror.WriteError(w, req, err)

		return
	}

	// pagination.Slice's preconditions hold because NewResource enforces them:
	// items are sorted by ID and IDs are unique and non-empty. Without the
	// non-empty guarantee Next would mint no cursor and the listing would report
	// itself complete with rows still unread.
	page, next := pagination.Slice(r.pages, r.items, params, req.URL, func(it Item) string {
		return it.ID
	})

	writeJSON(w, pagination.NewPage(page, next))
}

// get serves one item, or a problem+json 404.
func (r *Resource) get(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")

	for _, item := range r.items {
		if item.ID == id {
			writeJSON(w, item)

			return
		}
	}

	apierror.WriteError(w, req, apierror.NotFound("item "+id))
}

// writeJSON renders a successful representation. A failed write is not
// actionable — the status is already committed and API-010 records what was
// sent — and the ETag wrapper buffers these writes anyway.
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(payload)
}
