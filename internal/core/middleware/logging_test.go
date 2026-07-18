/*
Testing: logging.go

Pending:

Tested:
  Logging (via RequestID → Logging chain, and standalone)
    - TestLogging_ShouldEmitRequestCompleted: API-010 with status/bytes, caller/request groups, catalog conformance.
    - TestLogging_ShouldGiveSkipTheResponseStatus: the filter sees the status, so failures on a skipped path are logged.
    - TestLogging_ShouldSkipWhenPredicateMatches: skipped requests emit nothing.
    - TestLogging_ShouldSeedRemoteAddrWithoutRequestID: standalone Logging seeds remoteAddr and does not panic.

Tested elsewhere:

Declined:

Additional Remarks:
  Installs the global emitter hook via the recorder, so these tests run
  sequentially. API-010 is an ExternalSource event, normally emitted behind
  RequestID; the standalone case verifies the remoteAddr seeding fallback.
*/

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogging_ShouldEmitRequestCompleted(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})
	h := Chain(handler, RequestID, Logging(nil))
	rw := httptest.NewRecorder()

	// ACT
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/things", nil))

	// ASSERT
	rec.AssertEmitted(t, catalog.API010)
	rec.AssertData(t, catalog.API010, "status", int64(http.StatusCreated))
	rec.AssertData(t, catalog.API010, "bytesWritten", int64(len("hello")))

	got := rec.FindByID(catalog.API010)
	require.Len(t, got, 1)
	assert.Equal(t, rw.Header().Get("X-Request-Id"), got[0].Request("requestId"))
	assert.Equal(t, http.MethodGet, got[0].Request("method"))

	rec.AssertMatchesCatalog(t)
}

func TestLogging_ShouldGiveSkipTheResponseStatus(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE — a filter that suppresses routine traffic but keeps failures on
	// the same path, which is what the health-poll filter needs.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	var seen int

	skip := func(_ *http.Request, status int) bool {
		seen = status

		return status == http.StatusOK
	}

	failing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	// ACT
	Chain(failing, RequestID, Logging(skip)).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil),
	)

	// ASSERT — the failure on a normally-skipped path is still audited.
	assert.Equal(t, http.StatusServiceUnavailable, seen)
	rec.AssertEmitted(t, catalog.API010)
}

func TestLogging_ShouldSkipWhenPredicateMatches(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	skip := func(r *http.Request, _ int) bool { return r.URL.Path == "/skipme" }
	h := Chain(handler, RequestID, Logging(skip))
	rw := httptest.NewRecorder()

	// ACT
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/skipme", nil))

	// ASSERT
	rec.AssertNotEmitted(t, catalog.API010)
}

func TestLogging_ShouldSeedRemoteAddrWithoutRequestID(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE — Logging alone, without RequestID populating the context.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Logging(nil)(handler)
	rw := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)

	// ACT
	require.NotPanics(t, func() {
		h.ServeHTTP(rw, req)
	})

	// ASSERT — the ExternalSource guard is satisfied by seeding remoteAddr.
	rec.AssertEmitted(t, catalog.API010)

	got := rec.FindByID(catalog.API010)
	require.Len(t, got, 1)
	assert.Equal(t, req.RemoteAddr, got[0].Caller("remoteAddr"))
}
