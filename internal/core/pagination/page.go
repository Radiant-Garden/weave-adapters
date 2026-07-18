package pagination

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// Query parameter names, exported so handlers, tests, and the OpenAPI
// components in api/common/ all name them from one place.
const (
	// ParamPageSize caps how many items a page may carry.
	ParamPageSize = "pageSize"
	// ParamPageToken resumes a listing where the previous page ended.
	ParamPageToken = "pageToken"
)

// Client-facing validation messages. Both are returned verbatim in the
// errors[] extension, so they describe the expectation and never internal
// state.
const (
	// invalidCursorMessage covers every way a pageToken can be unreadable. It
	// names the only recovery there is — drop the token and list from the start
	// — because no other action differs between those ways.
	//
	// Named for the cursor rather than the token because gosec's G101 reads any
	// const whose name contains "token" as a possible hardcoded credential.
	// The string really is about the cursor, so an accurate name beats a nolint.
	invalidCursorMessage = "must be a nextPageToken returned by this endpoint; omit it to start from the first page"

	// atLeastOneMessage covers a pageSize that is a number but not a usable
	// one. Shared so the zero, negative, and negative-overflow paths cannot
	// drift into three phrasings of the same complaint.
	atLeastOneMessage = "must be at least 1"
)

// Paginator holds one collection's pagination rules. Declare one beside the
// handler that lists that collection:
//
//	var leasePages = pagination.New("leases", 100, 500)
//
// Binding the scope to the paginator rather than passing it per call is what
// makes a token minted for one collection un-parseable by another: there is no
// call site where the two can be given different values by mistake.
type Paginator struct {
	scope       string
	defaultSize int
	maxSize     int
}

// New returns a Paginator for a collection. scope is a stable name for that
// collection — it travels inside every token, so renaming it invalidates
// outstanding tokens, which is the intended effect when a listing's shape or
// order changes.
//
// It panics on a nonsensical configuration (empty scope, non-positive sizes, a
// default above the max). Like the event registry, these are wiring mistakes
// that should surface the first time the process starts rather than as a
// mis-clamped page in production.
func New(scope string, defaultSize, maxSize int) Paginator {
	switch {
	case scope == "":
		panic("pagination: scope must not be empty")
	case defaultSize < 1:
		panic(fmt.Sprintf("pagination: default page size must be positive, got %d", defaultSize))
	case maxSize < 1:
		panic(fmt.Sprintf("pagination: max page size must be positive, got %d", maxSize))
	case defaultSize > maxSize:
		panic(fmt.Sprintf("pagination: default page size %d exceeds max %d", defaultSize, maxSize))
	}

	return Paginator{scope: scope, defaultSize: defaultSize, maxSize: maxSize}
}

// Params are the resolved pagination inputs for one request: a page size
// already clamped to this collection's limits, and the key to resume after.
type Params struct {
	// Size is how many items the page may carry. Always between 1 and the
	// collection's max, whatever the client asked for.
	Size int
	// After is the key of the last item on the previous page; "" starts at the
	// beginning. A handler lists items ordered after this key.
	After string
}

// Parse resolves the pagination query parameters, returning an
// *apierror.Error (a 400 problem+json) if the client sent something invalid.
//
// Both parameters are checked before returning, so a request that gets both
// wrong is told about both at once — the convention that a client fixes every
// field in one round trip rather than one per attempt.
//
// The two failure modes are treated asymmetrically on purpose. An out-of-range
// pageSize is *clamped*: the client asked for more than this endpoint serves,
// and returning fewer items than requested is already something every caller
// must handle, since nextPageToken is the authority on whether more exist. A
// pageSize that is not a positive integer is *rejected*: there is no honest
// reading of "abc" or "-5" to clamp toward, and guessing would turn a client
// bug into a silently different query.
//
// A repeated parameter takes its first value, as net/http and every OpenAPI
// default do. Rejecting the repeat would be more consistent with the paragraph
// above, but a client that harmlessly appends a parameter twice is not making
// the kind of mistake worth a 400, and "first wins" is the behaviour every
// intermediary already assumes.
func (p Paginator) Parse(query url.Values) (Params, error) {
	var fieldErrors []apierror.FieldError

	size, sizeErr := p.parseSize(query.Get(ParamPageSize))
	if sizeErr != "" {
		fieldErrors = append(fieldErrors, apierror.FieldError{Field: ParamPageSize, Message: sizeErr})
	}

	after, ok := p.parseToken(query.Get(ParamPageToken))
	if !ok {
		fieldErrors = append(fieldErrors, apierror.FieldError{Field: ParamPageToken, Message: invalidCursorMessage})
	}

	if len(fieldErrors) > 0 {
		return Params{}, apierror.Validation(fieldErrors...)
	}

	return Params{Size: size, After: after}, nil
}

// parseSize resolves pageSize, returning a message describing the failure when
// the value cannot be used at all. Out-of-range values clamp and succeed.
func (p Paginator) parseSize(raw string) (int, string) {
	if raw == "" {
		return p.defaultSize, ""
	}

	size, err := strconv.Atoi(raw)

	// A number too large for an int is still an unambiguous "more than you can
	// have", so it clamps like any other oversized request. Letting it fall
	// through to the syntax error below would make the 400/clamp boundary the
	// platform's int width — 5000 clamped while 1e20 was rejected, for the same
	// client intent.
	if errors.Is(err, strconv.ErrRange) {
		if strings.HasPrefix(raw, "-") {
			return 0, atLeastOneMessage
		}

		return p.maxSize, ""
	}

	if err != nil {
		return 0, "must be an integer"
	}

	if size < 1 {
		return 0, atLeastOneMessage
	}

	return min(size, p.maxSize), ""
}

// parseToken resolves pageToken. An absent token is the first page, not a
// failure — ok is false only for a token that was sent and cannot be read.
func (p Paginator) parseToken(raw string) (after string, ok bool) {
	if raw == "" {
		return "", true
	}

	return decodeToken(raw, p.scope)
}

// NextToken mints the token that resumes after key. Pass the key of the last
// item on the page just built.
//
// An empty key returns an empty token, which NewPage renders as an absent
// nextPageToken — "there is no next page". That makes the degenerate case
// compose: a handler can write
//
//	pagination.NewPage(items, pages.NextToken(lastKey))
//
// and an empty listing yields a correct last-page envelope without a special
// case. It also means a token carrying no key is never minted, which is why
// decoding one is a rejection rather than a silent restart.
func (p Paginator) NextToken(key string) string {
	if key == "" {
		return ""
	}

	return encodeToken(p.scope, key)
}

// Page is the collection envelope every list endpoint returns.
type Page[T any] struct {
	Items []T `json:"items"`
	// NextPageToken is absent on the last page. Its presence — not a full page
	// of items — is what tells a client to ask again.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// NewPage builds the envelope. Pass "" as nextPageToken for the last page.
func NewPage[T any](items []T, nextPageToken string) Page[T] {
	return Page[T]{Items: items, NextPageToken: nextPageToken}
}

// MarshalJSON renders the envelope, normalizing a nil Items to an empty array.
//
// The normalization lives here rather than in NewPage because Page and its
// fields are exported: a handler that builds the struct literally, or that
// returns a zero Page on an early exit, would otherwise emit `"items": null`.
// Clients iterate that field directly, so a null there is the difference
// between "no leases" and a null-dereference in the caller — too sharp an edge
// to leave guarded by one constructor nobody is forced to use.
func (p Page[T]) MarshalJSON() ([]byte, error) {
	if p.Items == nil {
		p.Items = []T{}
	}

	// payload sheds this method, so marshalling it does not recurse.
	type payload Page[T]

	return json.Marshal(payload(p))
}
