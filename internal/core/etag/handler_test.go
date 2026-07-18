/*
Testing: handler.go

Pending:

Tested:
  Conditional
    - TestConditional_ShouldTagASuccessfulResponse: 200 carries an ETag and the body.
    - TestConditional_ShouldAnswer304WhenTagMatches: the polling case, with no body.
    - TestConditional_ShouldSendTheBodyWhenTagDiffers: a changed representation is not a 304.
    - TestConditional_ShouldMatchWeakAndListedTags: weak forms and multi-value headers hit.
    - TestConditional_ShouldNotTagErrorResponses: a 404 is passed through untagged.
    - TestConditional_ShouldPassThroughNonReadMethods: If-None-Match means something else there.
    - TestConditional_ShouldPreserveHandlerHeaders: Cache-Control and friends survive.
    - TestConditional_ShouldTrackTheRepresentationAcrossChanges: the tag follows the data.
    - TestConditional_ShouldHandleHeadRequests: HEAD is conditional too.
    - TestConditional_ShouldTagAnEmptyBody: a handler that writes nothing still tags.

Tested elsewhere:
  Tag computation and If-None-Match parsing are covered in etag_test.go.

Declined:
  captureWriter.Unwrap — a one-line accessor; the buffering it sits behind is
  what every test here exercises.

Additional Remarks:
  The 304 assertions go through a real listener as well as a recorder:
  httptest.ResponseRecorder happily records a body on a 304, so a
  recorder-only test would pass even if the body were written to the wire.
*/

package etag

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const representation = `{"items":[{"id":"lease-1"}],"nextPageToken":""}`

// jsonHandler returns a handler that writes body as JSON.
func jsonHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

// call runs a request through Conditional with an optional If-None-Match.
func call(t *testing.T, h http.Handler, method, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/leases", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	rec := httptest.NewRecorder()
	Conditional(h).ServeHTTP(rec, req)

	return rec
}

func TestConditional_ShouldTagASuccessfulResponse(t *testing.T) {
	t.Parallel()

	// ACT
	resp := call(t, jsonHandler(representation), http.MethodGet, "")

	// ASSERT
	require.Equal(t, http.StatusOK, resp.Code)
	// Byte equality, not JSONEq: a re-serialization with the same meaning but
	// different bytes is exactly the change an ETag exists to detect, so an
	// equivalence assertion would pass on the one case that matters.
	assert.Equal(t, representation, resp.Body.String()) //nolint:testifylint // byte equality is the point
	assert.Equal(t, Compute([]byte(representation)), resp.Header().Get("ETag"))
}

func TestConditional_ShouldAnswer304WhenTagMatches(t *testing.T) {
	t.Parallel()

	// ARRANGE — the second poll of an unchanged collection.
	tag := Compute([]byte(representation))

	// ACT
	resp := call(t, jsonHandler(representation), http.MethodGet, tag)

	// ASSERT — this is the whole point: weave polls and pays for a status line.
	require.Equal(t, http.StatusNotModified, resp.Code)
	assert.Empty(t, resp.Body.String())
	assert.Equal(t, tag, resp.Header().Get("ETag"), "a 304 must still carry the tag")
}

func TestConditional_ShouldNotWriteABodyOnTheWireFor304(t *testing.T) {
	t.Parallel()

	// ARRANGE — httptest.ResponseRecorder records a body even when the real
	// server would refuse it, so this one goes over a listener.
	tag := Compute([]byte(representation))

	ts := httptest.NewServer(Conditional(jsonHandler(representation)))
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", tag)

	// ACT
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// ASSERT
	require.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Empty(t, body)
	assert.Empty(t, resp.Header.Get("Content-Length"), "net/http omits the length for a 304")
}

func TestConditional_ShouldSendTheBodyWhenTagDiffers(t *testing.T) {
	t.Parallel()

	// ACT — the client holds a stale tag.
	resp := call(t, jsonHandler(representation), http.MethodGet, `"stale"`)

	// ASSERT
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Equal(t, representation, resp.Body.String()) //nolint:testifylint // byte equality, see above
}

func TestConditional_ShouldMatchWeakAndListedTags(t *testing.T) {
	t.Parallel()

	tag := Compute([]byte(representation))

	tests := []struct {
		name        string
		ifNoneMatch string
	}{
		{name: "should hit on a weak form", ifNoneMatch: "W/" + tag},
		{name: "should hit within a list", ifNoneMatch: `"other", ` + tag},
		{name: "should hit on the wildcard", ifNoneMatch: "*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			resp := call(t, jsonHandler(representation), http.MethodGet, tt.ifNoneMatch)

			// ASSERT
			assert.Equal(t, http.StatusNotModified, resp.Code)
		})
	}
}

func TestConditional_ShouldNotTagErrorResponses(t *testing.T) {
	t.Parallel()

	// ARRANGE — an error has no stable identity worth caching.
	failing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"weave-adapters:not-found"}`))
	})

	// ACT
	resp := call(t, failing, http.MethodGet, "*")

	// ASSERT — the status and body pass through, and no tag invites the client
	// to cache the failure or to receive a 304 for one.
	require.Equal(t, http.StatusNotFound, resp.Code)
	assert.Equal(t, `{"type":"weave-adapters:not-found"}`, resp.Body.String()) //nolint:testifylint // byte equality, see above
	assert.Empty(t, resp.Header().Get("ETag"))
}

func TestConditional_ShouldPassThroughNonReadMethods(t *testing.T) {
	t.Parallel()

	tests := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, method := range tests {
		t.Run("should pass through "+method, func(t *testing.T) {
			t.Parallel()

			// ACT — If-None-Match on a write means optimistic concurrency
			// (M3's write side), not "send me a 304".
			resp := call(t, jsonHandler(representation), method, "*")

			// ASSERT
			assert.Equal(t, http.StatusOK, resp.Code)
			assert.Equal(t, representation, resp.Body.String()) //nolint:testifylint // byte equality, see above
			assert.Empty(t, resp.Header().Get("ETag"))
		})
	}
}

func TestConditional_ShouldPreserveHandlerHeaders(t *testing.T) {
	t.Parallel()

	// ARRANGE
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Cache", "miss")
		_, _ = w.Write([]byte(representation))
	})

	// ACT
	resp := call(t, handler, http.MethodGet, "")

	// ASSERT — buffering the body must not swallow what the handler set.
	assert.Equal(t, "application/json", resp.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header().Get("Cache-Control"))
	assert.Equal(t, "miss", resp.Header().Get("X-Cache"))
}

func TestConditional_ShouldTrackTheRepresentationAcrossChanges(t *testing.T) {
	t.Parallel()

	// ARRANGE — a resource that changes between polls.
	body := representation
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	first := call(t, handler, http.MethodGet, "")
	tag := first.Header().Get("ETag")

	// ACT — poll again with the tag, then change the data and poll once more.
	unchanged := call(t, handler, http.MethodGet, tag)

	body = `{"items":[{"id":"lease-1"},{"id":"lease-2"}],"nextPageToken":""}`
	changed := call(t, handler, http.MethodGet, tag)

	// ASSERT — a stale tag must never yield a 304, or the client keeps serving
	// data that no longer exists.
	assert.Equal(t, http.StatusNotModified, unchanged.Code)
	assert.Equal(t, http.StatusOK, changed.Code)
	assert.NotEqual(t, tag, changed.Header().Get("ETag"))
	assert.Equal(t, body, changed.Body.String())
}

func TestConditional_ShouldHandleHeadRequests(t *testing.T) {
	t.Parallel()

	tag := Compute([]byte(representation))

	// ACT
	tagged := call(t, jsonHandler(representation), http.MethodHead, "")
	conditional := call(t, jsonHandler(representation), http.MethodHead, tag)

	// ASSERT — HEAD is a read, so it is tagged and conditional like GET.
	assert.Equal(t, tag, tagged.Header().Get("ETag"))
	assert.Equal(t, http.StatusNotModified, conditional.Code)
}

func TestConditional_ShouldTagAnEmptyBody(t *testing.T) {
	t.Parallel()

	// ARRANGE — a handler that sets a status and writes nothing.
	empty := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	// ACT
	first := call(t, empty, http.MethodGet, "")
	second := call(t, empty, http.MethodGet, first.Header().Get("ETag"))

	// ASSERT — an empty representation is still a representation.
	assert.Equal(t, Compute(nil), first.Header().Get("ETag"))
	assert.Equal(t, http.StatusNotModified, second.Code)
}
