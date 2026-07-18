/*
Testing: demo.go

Pending:

Tested:
  NewResource / Mount / Handler / list / get
    - TestDemo_ShouldServeAnAuthenticatedListWithAnETag: the happy path, tagged.
    - TestDemo_ShouldAnswer304OnARepoll: the polling loop weave actually runs.
    - TestDemo_ShouldWalkEveryPageByToken: the token cursor reaches the last page and stops.
    - TestDemo_ShouldWalkEveryPageByLink: the link cursor walks the same items, as weave pages.
    - TestDemo_ShouldRejectAnUnauthenticatedRequest: 401 problem+json correlated with its header.
    - TestDemo_ShouldAttributeTheRequestToTheAuthenticatedCaller: the token label reaches API-010's caller.subject.
    - TestDemo_ShouldRejectAMalformedPageToken: 400 problem+json, never a silent restart.
    - TestDemo_ShouldAnswer404ForAnUnknownItem: problem+json from a handler, not the router.
    - TestDemo_ShouldTagEachPageDistinctly: a stale page's ETag cannot validate a different page.
  package placement
    - TestDemo_ShouldNotBeReachableFromTheBinary: the resource is test-only, enforced not assumed.

Tested elsewhere:
  Each mechanism is unit-tested where it lives — auth, etag, pagination and
  apierror all have their own suites. Nothing here re-tests them in isolation.

Declined:
  writeJSON — a two-line helper with no branch; every assertion below runs
  through it.

Additional Remarks:
  This file is M2's exit gate, so it asserts the COMPOSITION rather than the
  parts: that auth populates a caller the rest of the chain sees, that an ETag
  is computed over the body pagination actually produced, and that every error
  shape survives the real middleware chain. A test here that passed while the
  parts were individually correct but did not fit together would defeat the
  purpose.

  The chain comes from httpserver.NewHandler, the same call the server makes.
  Assembling a lookalike chain here would prove nothing about the one that ships.

  Tests that install the event recorder mutate the process-global emitter hook
  and cannot run in parallel.
*/

package httptest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/auth"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/radiantgarden/weave-adapters/internal/core/httpserver"
	"github.com/radiantgarden/weave-adapters/internal/core/pagination"
)

// demoToken is the credential the tests present. Only its hash reaches the
// verifier, exactly as a real deployment stores it.
const demoToken = "wadapt_demo"

// newTestHandler returns the demo resource behind the production chain with
// authentication enabled, plus five items — enough to span three pages at the
// demo's page size of two, so the last page is genuinely short.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()

	verifier := auth.NewVerifier([]auth.Entry{{
		Label:     "weave-test",
		Hash:      auth.Hash(demoToken),
		CreatedAt: time.Date(2026, time.July, 18, 0, 0, 0, 0, time.UTC),
	}})

	resource := NewResource(
		Item{ID: "item-1", Name: "first"},
		Item{ID: "item-2", Name: "second"},
		Item{ID: "item-3", Name: "third"},
		Item{ID: "item-4", Name: "fourth"},
		Item{ID: "item-5", Name: "fifth"},
	)

	return resource.Handler(auth.Bearer(verifier, httpserver.Unauthenticated))
}

// get issues an authenticated GET unless headers say otherwise.
func get(t *testing.T, handler http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+demoToken)

	for name, value := range headers {
		if value == "" {
			req.Header.Del(name)

			continue
		}

		req.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	return recorder
}

// decodePage decodes a collection response.
func decodePage(t *testing.T, body []byte) pagination.Page[Item] {
	t.Helper()

	var page pagination.Page[Item]
	require.NoError(t, json.Unmarshal(body, &page))

	return page
}

// decodeProblem decodes a problem+json response.
func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) apierror.Problem {
	t.Helper()

	require.Equal(t, apierror.ContentType, recorder.Header().Get("Content-Type"))

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))

	return problem
}

func TestDemo_ShouldServeAnAuthenticatedListWithAnETag(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	recorder := get(t, newTestHandler(t), CollectionPath, nil)

	// ASSERT — a tagged, paginated collection, which is the shape every adapter
	// list endpoint owes weave.
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.NotEmpty(t, recorder.Header().Get("ETag"))
	assert.NotEmpty(t, recorder.Header().Get("X-Request-Id"))

	page := decodePage(t, recorder.Body.Bytes())
	assert.Len(t, page.Items, DefaultPageSize)
	assert.Equal(t, "item-1", page.Items[0].ID)

	// Both cursor forms, together, because more items remain.
	assert.NotEmpty(t, page.NextPageToken)
	assert.NotEmpty(t, page.NextPageURL)
}

func TestDemo_ShouldAnswer304OnARepoll(t *testing.T) {
	t.Parallel()

	// ARRANGE — the first poll, and the tag it handed back.
	handler := newTestHandler(t)
	first := get(t, handler, CollectionPath, nil)
	require.Equal(t, http.StatusOK, first.Code)

	tag := first.Header().Get("ETag")
	require.NotEmpty(t, tag)

	// ACT — the same request again, as weave polls on an interval.
	second := get(t, handler, CollectionPath, map[string]string{"If-None-Match": tag})

	// ASSERT — this is the whole point of the ETag: an unchanged collection
	// costs a status line instead of a body.
	assert.Equal(t, http.StatusNotModified, second.Code)
	assert.Empty(t, second.Body.Bytes())
	assert.Equal(t, tag, second.Header().Get("ETag"))
}

func TestDemo_ShouldWalkEveryPageByToken(t *testing.T) {
	t.Parallel()

	// ARRANGE
	handler := newTestHandler(t)

	// ACT — follow nextPageToken until the listing says stop.
	var (
		seen   []string
		target = CollectionPath
		pages  int
	)

	for {
		pages++
		require.LessOrEqual(t, pages, 10, "the walk should terminate, not loop")

		recorder := get(t, handler, target, nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		page := decodePage(t, recorder.Body.Bytes())
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}

		if page.NextPageToken == "" {
			// The last page carries neither cursor form.
			assert.Empty(t, page.NextPageURL)

			break
		}

		target = CollectionPath + "?" + pagination.ParamPageToken + "=" + page.NextPageToken
	}

	// ASSERT — every item exactly once, in order, across three pages of 2/2/1.
	assert.Equal(t, []string{"item-1", "item-2", "item-3", "item-4", "item-5"}, seen)
	assert.Equal(t, 3, pages)
}

func TestDemo_ShouldWalkEveryPageByLink(t *testing.T) {
	t.Parallel()

	// ARRANGE — the same walk weave performs: follow the link, never assemble a
	// request from the token.
	handler := newTestHandler(t)

	var (
		seen   []string
		target = CollectionPath
		pages  int
	)

	// ACT
	for {
		pages++
		require.LessOrEqual(t, pages, 10, "the walk should terminate, not loop")

		recorder := get(t, handler, target, nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		page := decodePage(t, recorder.Body.Bytes())
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}

		if page.NextPageURL == "" {
			break
		}

		// The link must be usable as-is. A relative reference is what weave
		// resolves against its own base, so anything absolute would send it
		// somewhere else entirely.
		require.True(t, strings.HasPrefix(page.NextPageURL, "/"), "got %q", page.NextPageURL)
		require.False(t, strings.HasPrefix(page.NextPageURL, "//"), "got %q", page.NextPageURL)

		target = page.NextPageURL
	}

	// ASSERT — the two cursor forms address the same listing.
	assert.Equal(t, []string{"item-1", "item-2", "item-3", "item-4", "item-5"}, seen)
	assert.Equal(t, 3, pages)
}

func TestDemo_ShouldCarryFiltersAcrossPagesInTheLink(t *testing.T) {
	t.Parallel()

	// ARRANGE — a query parameter the endpoint does not interpret. It still has
	// to survive into the link, because a link-following client sends nothing
	// else, and a real filter dropped here would silently widen later pages.
	handler := newTestHandler(t)

	// ACT
	recorder := get(t, handler, CollectionPath+"?scopeId=10.0.0.0&pageSize=2", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	page := decodePage(t, recorder.Body.Bytes())

	// ASSERT
	require.NotEmpty(t, page.NextPageURL)
	assert.Contains(t, page.NextPageURL, "scopeId=10.0.0.0")
	assert.Contains(t, page.NextPageURL, "pageSize=2")
}

func TestDemo_ShouldTagEachPageDistinctly(t *testing.T) {
	t.Parallel()

	// ARRANGE — page one and its tag.
	handler := newTestHandler(t)

	first := get(t, handler, CollectionPath, nil)
	require.Equal(t, http.StatusOK, first.Code)

	page := decodePage(t, first.Body.Bytes())
	require.NotEmpty(t, page.NextPageToken)

	// ACT — page two, presenting page one's tag.
	second := get(t, handler, CollectionPath+"?"+pagination.ParamPageToken+"="+page.NextPageToken,
		map[string]string{"If-None-Match": first.Header().Get("ETag")})

	// ASSERT — the tag covers the representation, not the route. Answering 304
	// here would hand a client page one's body while it believed it had page
	// two, which is the failure mode that makes ETag-plus-pagination worth
	// proving together rather than apart.
	require.Equal(t, http.StatusOK, second.Code)
	assert.NotEqual(t, first.Header().Get("ETag"), second.Header().Get("ETag"))
	assert.Equal(t, "item-3", decodePage(t, second.Body.Bytes()).Items[0].ID)
}

func TestDemo_ShouldRejectAnUnauthenticatedRequest(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — no Authorization header at all.
	recorder := get(t, newTestHandler(t), CollectionPath, map[string]string{"Authorization": ""})

	// ASSERT — problem+json, not a bare 401...
	require.Equal(t, http.StatusUnauthorized, recorder.Code)

	problem := decodeProblem(t, recorder)
	assert.Equal(t, http.StatusUnauthorized, problem.Status)
	assert.Equal(t, apierror.TypeFor("unauthorized"), problem.Type)
	assert.Equal(t, CollectionPath, problem.Instance)

	// ...and correlated: the body's requestId is the header an operator greps.
	assert.NotEmpty(t, problem.RequestID)
	assert.Equal(t, recorder.Header().Get("X-Request-Id"), problem.RequestID)

	// The rejection reveals nothing about the collection.
	assert.NotContains(t, recorder.Body.String(), "item-")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestDemo_ShouldAttributeTheRequestToTheAuthenticatedCaller(t *testing.T) {
	// ARRANGE — the audit line every request produces.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	recorder := get(t, newTestHandler(t), CollectionPath, nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	// ASSERT — auth runs inside logging precisely so the completed-request
	// event carries who made it. The token label is what fills caller.subject,
	// and until auth landed nothing ever did: an API-010 line with an empty
	// subject is the audit gap this composition exists to close.
	rec.AssertEmitted(t, catalog.API010)

	emitted := rec.All()
	require.NotEmpty(t, emitted)

	var subjects []any

	for _, event := range emitted {
		if event.ID == catalog.API010 {
			subjects = append(subjects, event.Caller("subject"))
		}
	}

	require.NotEmpty(t, subjects, "the list request should have produced an audit line")
	assert.Equal(t, []any{"weave-test"}, subjects)
}

func TestDemo_ShouldRejectAMalformedPageToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, token string
	}{
		{name: "should reject junk", token: "not-a-token"},
		{name: "should reject a token from another collection", token: pagination.New("other", 2, 10).NextToken("x")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			recorder := get(t, newTestHandler(t), CollectionPath+"?"+pagination.ParamPageToken+"="+tt.token, nil)

			// ASSERT — a 400 naming the field, never a silent restart from page
			// one dressed up as a successful listing.
			require.Equal(t, http.StatusBadRequest, recorder.Code)

			problem := decodeProblem(t, recorder)
			require.Len(t, problem.Errors, 1)
			assert.Equal(t, pagination.ParamPageToken, problem.Errors[0].Field)
			assert.NotContains(t, recorder.Body.String(), "item-1")
		})
	}
}

func TestDemo_ShouldAnswer404ForAnUnknownItem(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	recorder := get(t, newTestHandler(t), CollectionPath+"/item-999", nil)

	// ASSERT — from the handler, through apierror, in the same shape the router
	// and the auth middleware produce.
	require.Equal(t, http.StatusNotFound, recorder.Code)

	problem := decodeProblem(t, recorder)
	assert.Equal(t, apierror.TypeFor("not-found"), problem.Type)
	assert.Equal(t, "/api/v1/items/item-999", problem.Instance)
	assert.Equal(t, recorder.Header().Get("X-Request-Id"), problem.RequestID)
}

func TestDemo_ShouldNotBeReachableFromTheBinary(t *testing.T) {
	t.Parallel()

	// ARRANGE — every package the shipped binary links in.
	const binary = "github.com/radiantgarden/weave-adapters/cmd/weave-adapter-dhcp-windows"

	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", binary).CombinedOutput()
	require.NoError(t, err, "go list failed: %s", out)

	// ACT
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// ASSERT — the demo resource is test-only by decision, and a decision no
	// build enforces is a comment. Mounting it would give weave a route that
	// serves fixtures.
	assert.NotContains(t, deps, "github.com/radiantgarden/weave-adapters/internal/core/httptest")
	require.Greater(t, len(deps), 10, "go list should report the real dependency set, got %q", out)
}

// TestDemo_ShouldSpanSeveralPages guards the fixture itself: the walk tests
// only prove pagination if the collection is larger than one page.
func TestDemo_ShouldSpanSeveralPages(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Greater(t, 5, DefaultPageSize, "the fixture must not fit on one page")
	assert.Equal(t, 3, 1+(5-1)/DefaultPageSize, "five items at size "+strconv.Itoa(DefaultPageSize)+" is three pages")
}
