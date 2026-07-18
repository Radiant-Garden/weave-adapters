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
    - TestProblemErrors_ShouldClearStaleContentLength: a handler-declared length cannot truncate the JSON.
    - TestProblemErrors_ShouldPreserveHandlerHeaders: Retry-After and friends survive the rewrite.
    - TestProblemErrors_ShouldRewriteHeadRequests: HEAD keeps the status and content type.
    - TestProblemErrors_ShouldNotDoubleHandleParameterisedContentType: a charset suffix still counts as ours.
    - TestProblemErrors_ShouldTruncateAnOverlongPath: an attacker-sized path is bounded before it is echoed.
    - TestProblemErrors_ShouldAllowFlushingThroughResponseController: streaming survives three wrappers.

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
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
func TestProblemErrors_ShouldClearStaleContentLength(t *testing.T) {
	// ARRANGE — a handler that sizes its own 404 body, as a proxying or
	// pre-rendering handler would. Served over a real listener: httptest's
	// recorder does not enforce Content-Length, so a recorder-based test would
	// pass while the wire truncated.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		const short = "nope"

		w.Header().Set("Content-Length", strconv.Itoa(len(short)))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(short))
	}))

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// ACT
	resp, err := ts.Client().Get(ts.URL + "/x") //nolint:noctx // test-local server
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// ASSERT — a stale length would truncate the longer JSON to four bytes, or
	// to nothing, leaving the client a response it cannot parse at all.
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(body, &problem), "body was truncated: %q", string(body))
	assert.Equal(t, "weave-adapters:not-found", problem.Type)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldPreserveHandlerHeaders(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
	}))

	// ACT
	resp := through(t, handler, http.MethodGet, "/x")

	// ASSERT — only the body and its content type are replaced. Resetting the
	// header map would be an easy way to clear Content-Length and would drop
	// these silently.
	assert.Equal(t, "120", resp.Header().Get("Retry-After"))
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
	assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldRewriteHeadRequests(t *testing.T) {
	// ARRANGE — net/http suppresses the body for HEAD but keeps the headers.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	resp := routed(t, http.MethodHead, "/api/v1/nope")

	// ASSERT — status and content type still describe a problem document.
	assert.Equal(t, http.StatusNotFound, resp.Code)
	assert.Equal(t, apierror.ContentType, resp.Header().Get("Content-Type"))
	rec.AssertEmitted(t, catalog.API900)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldNotDoubleHandleParameterisedContentType(t *testing.T) {
	// ARRANGE — the conventional spelling of the media type, with a charset.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := ProblemErrors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", apierror.ContentType+"; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"weave-adapters:not-found","detail":"already rendered"}`))
	}))

	// ACT
	resp := through(t, handler, http.MethodGet, "/x")

	// ASSERT — a string compare would treat this as router-generated, emit a
	// second event, and overwrite the specific detail with the generic one.
	assert.Contains(t, resp.Body.String(), "already rendered")
	assert.Empty(t, rec.All(), "an already-rendered problem must not be re-emitted")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldTruncateAnOverlongPath(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	longPath := "/api/v1/" + strings.Repeat("a", 4000)

	// ACT
	resp := routed(t, http.MethodGet, longPath)

	// ASSERT — the path is attacker-controlled; echoing it whole would copy
	// kilobytes into both the response and log storage on every request.
	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &problem))
	assert.Less(t, len(problem.Detail), 200)
	assert.Contains(t, problem.Detail, "…")

	// Instance carries the path too, and truncating only the detail leaves the
	// amplification this bound exists to prevent fully intact.
	assert.LessOrEqual(t, len(problem.Instance), apierror.MaxReflectedPathLen+len("…"))
	assert.Contains(t, problem.Instance, "…")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestProblemErrors_ShouldAllowFlushingThroughResponseController(t *testing.T) {
	// ARRANGE — a streaming handler, as the planned SSE endpoint would be. It
	// must reach Flush through http.ResponseController: three wrappers sit
	// between it and the real writer, and none implement Flusher directly.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// Buffered channel rather than a captured variable: the handler runs on the
	// server's goroutine, so a plain assignment would be a data race.
	flushed := make(chan error, 1)

	handler := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("chunk"))
			flushed <- http.NewResponseController(w).Flush()
		}),
		Recovery, RequestID, Logging(nil), ProblemErrors,
	)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// ACT
	resp, err := ts.Client().Get(ts.URL + "/stream") //nolint:noctx // test-local server
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	// ASSERT
	require.NoError(t, <-flushed, "Unwrap should keep Flush reachable through the chain")
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
