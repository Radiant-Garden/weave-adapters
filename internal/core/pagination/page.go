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

// Client-facing validation messages, returned verbatim in the errors[]
// extension. Shared so the paths that produce each cannot drift into several
// phrasings of one complaint.
//
// invalidCursorMessage is named for the cursor, not the token, because gosec's
// G101 reads any const whose name contains "token" as a possible credential.
const (
	invalidCursorMessage = "must be a nextPageToken returned by this endpoint; omit it to start from the first page"
	atLeastOneMessage    = "must be at least 1"
)

// Paginator holds one collection's pagination rules. Declare one beside the
// handler that lists that collection:
//
//	var leasePages = pagination.New("leases", 100, 500)
//
// Binding the scope here rather than passing it per call is what makes a token
// minted for one collection un-parseable by another.
type Paginator struct {
	scope       string
	defaultSize int
	maxSize     int
}

// New returns a Paginator for a collection. scope is a stable name for it and
// travels inside every token, so renaming it invalidates outstanding tokens.
//
// It panics on an unusable configuration (empty scope, non-positive sizes, a
// default above the max) so wiring mistakes surface at process start.
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

// Parse resolves the pagination query parameters, returning an *apierror.Error
// (a 400 problem+json) if the client sent something invalid. Both parameters
// are checked before returning, so a request that gets both wrong is told about
// both at once.
//
// A pageSize above the maximum is clamped; one that is not a positive integer
// is rejected, since there is no honest value to clamp "abc" or "-5" toward.
// A repeated parameter takes its first value, as net/http does.
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

	// Clamp rather than reject, so the 400/clamp boundary is not the platform's
	// int width: 5000 and 1e20 are the same client intent.
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

// NextToken mints the token that resumes after key. An empty key returns an
// empty token, so a token carrying no key is never minted — which is why
// decoding one is a rejection rather than a silent restart.
//
// Most handlers want Next, which mints this token and its link together.
func (p Paginator) NextToken(key string) string {
	if key == "" {
		return ""
	}

	return encodeToken(p.scope, key)
}

// NextPage is the cursor in both the forms a list response carries. One type
// rather than two strings, so the pair cannot be transposed or half-set.
type NextPage struct {
	// Token is the opaque cursor, echoed back as ?pageToken=.
	Token string
	// URL is the same cursor as a relative reference, for link-following
	// clients.
	URL string
}

// Next mints the next-page cursor from the current request URL and the key of
// the last item on the page just built. An empty key yields a zero NextPage,
// which renders as a last page.
//
// The link is always relative, so the adapter never has to derive its own
// external base URL from Host or X-Forwarded-* headers.
//
// It preserves every query parameter and replaces only pageToken. A
// link-following client sends nothing but the link, so dropping a filter here
// would serve a filtered first page and unfiltered later ones.
func (p Paginator) Next(requestURL *url.URL, key string) NextPage {
	token := p.NextToken(key)
	if token == "" || requestURL == nil {
		return NextPage{}
	}

	query := requestURL.Query()
	query.Set(ParamPageToken, token)

	// Path and RawQuery only: url.URL renders a relative reference when Scheme
	// and Host are empty.
	next := url.URL{Path: requestURL.Path, RawQuery: query.Encode()}

	return NextPage{Token: token, URL: next.String()}
}

// Page is the collection envelope every list endpoint returns.
type Page[T any] struct {
	Items []T `json:"items"`
	// NextPageToken is absent on the last page. Its presence — not a full page
	// of items — is what tells a client to ask again.
	NextPageToken string `json:"nextPageToken,omitempty"`
	// NextPageURL is the same cursor as a relative link, for clients that
	// follow links rather than echo tokens. Absent on the last page, always
	// alongside NextPageToken and never instead of it.
	NextPageURL string `json:"nextPageUrl,omitempty"`
}

// NewPage builds the envelope. Pass a zero NextPage for the last page:
//
//	pagination.NewPage(items, pages.Next(r.URL, lastKey))
func NewPage[T any](items []T, next NextPage) Page[T] {
	return Page[T]{Items: items, NextPageToken: next.Token, NextPageURL: next.URL}
}

// MarshalJSON renders the envelope, normalizing a nil Items to an empty array
// so the response never carries "items": null.
//
// It lives here rather than in NewPage because Page and its fields are
// exported: a struct literal or a zero Page returned on an early exit would
// otherwise bypass the guarantee.
func (p Page[T]) MarshalJSON() ([]byte, error) {
	if p.Items == nil {
		p.Items = []T{}
	}

	// payload sheds this method, so marshalling it does not recurse.
	type payload Page[T]

	return json.Marshal(payload(p))
}
