/*
Testing: problem.go

Pending:

Tested:
  ProblemErrors
    - TestProblemErrors_ShouldRewriteRouterNotFound: the mux's plain-text 404 becomes problem+json.
    - TestProblemErrors_ShouldRewriteRouterMethodNotAllowed: 405 too, preserving Allow.
    - TestProblemErrors_ShouldLeaveApierrorResponsesAlone: an already-rendered 404 is not double-handled.
    - TestProblemErrors_ShouldPassThroughOtherStatuses: success and other errors are untouched.
    - TestProblemErrors_ShouldNotAppendTheRouterBody: no plain text trails the JSON.
    - TestProblemErrors_ShouldIgnoreASecondWriteHeader: a duplicate status write is dropped.

Tested elsewhere:
  The end-to-end shape through the real chain is covered in
  httpserver/server_test.go.

Declined:
  problemWriter.Unwrap — a one-line accessor for http.ResponseController;
  exercised implicitly wherever a wrapped writer is used.

Additional Remarks:
  Requests carry a caller context because the API-9xx events are ExternalSource;
  in production the request-ID middleware supplies it.

  Tests install the event recorder, which mutates the global emitter hook, so
  they run sequentially.
*/

package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
)

// routed runs a request through ProblemErrors wrapping a mux that only knows
// GET /api/v1/health.
func routed(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return through(t, ProblemErrors(mux), method, path)
}

// through runs a request through h with a caller context attached.
func through(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	req = req.WithContext(events.WithCaller(req.Context(), events.Caller{
		RemoteAddr: "192.0.2.1:1234",
		RequestID:  "req-abc",
		Method:     method,
		Path:       path,
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldRewriteRouterNotFound(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	resp := routed(t, http.MethodGet, "/api/v1/nope")

	// ASSERT — the most common error a client meets must share the one shape.
	require.Equal(t, http.StatusNotFound, resp.Code)
	assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
	assert.Equal(t, "req-abc", problem.RequestID, "a router 404 should still correlate with the logs")
	assert.Contains(t, problem.Detail, "/api/v1/nope")

	rec.AssertEmitted(t, catalog.API900)
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldRewriteRouterMethodNotAllowed(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT — the path is registered, the method is not.
	resp := routed(t, http.MethodPost, "/api/v1/health")

	// ASSERT
	require.Equal(t, http.StatusMethodNotAllowed, resp.Code)
	assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))

	// The mux's Allow header must survive the rewrite — it is the only machine
	// -readable statement of what the route does accept.
	assert.Equal(t, "GET, HEAD", resp.Header().Get("Allow"))

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
	assert.Equal(t, "weave-adapters:method-not-allowed", problem.Type)
	assert.Contains(t, problem.Detail, http.MethodPost)

	rec.AssertEmitted(t, catalog.API902)
	rec.AssertData(t, catalog.API902, "allow", "GET, HEAD")
	rec.AssertMatchesCatalog(t)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldLeaveApierrorResponsesAlone(t *testing.T) {
	// ARRANGE — a handler that already renders its own 404 through apierror.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apierror.WriteError(w, r, apierror.NotFound("lease 10.0.0.5"))
	}))

	// ACT
	resp := through(t, handler, http.MethodGet, "/api/v1/leases/10.0.0.5")

	// ASSERT — rewriting here would emit a second event and replace a more
	// specific detail with a generic one.
	require.Equal(t, http.StatusNotFound, resp.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
	assert.Equal(t, "The requested lease 10.0.0.5 was not found.", problem.Detail)

	rec.AssertEmittedN(t, catalog.API900, 1)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldPassThroughOtherStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "should pass through a success", status: http.StatusOK, body: "fine"},
		{name: "should pass through a no-content", status: http.StatusNoContent},
		{name: "should pass through an unrelated error", status: http.StatusTeapot, body: "short and stout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)

				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))

			// ACT
			resp := through(t, handler, http.MethodGet, "/x")

			// ASSERT — only router 404/405 are rewritten; nothing else is touched.
			assert.Equal(t, tt.status, resp.Code)
			assert.Equal(t, tt.body, resp.Body.String())
			assert.Empty(t, rec.All())
		})
	}
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldNotAppendTheRouterBody(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	resp := routed(t, http.MethodGet, "/api/v1/nope")

	// ASSERT — the mux writes "404 page not found" after its WriteHeader; if it
	// were forwarded the body would be JSON followed by plain text, which no
	// client could parse.
	assert.NotContains(t, resp.Body.String(), "page not found")

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem),
		"the body should be exactly one JSON document")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldIgnoreASecondWriteHeader(t *testing.T) {
	// ARRANGE — a handler that writes its status twice, which net/http would
	// otherwise log as a superfluous call.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusNotFound)
	}))

	// ACT
	resp := through(t, handler, http.MethodGet, "/x")

	// ASSERT — the first status wins, and the late 404 does not trigger a rewrite.
	assert.Equal(t, http.StatusOK, resp.Code)
	assert.Empty(t, rec.All())
}
