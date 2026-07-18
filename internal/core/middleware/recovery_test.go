/*
Testing: recovery.go

Pending:

Tested:
  Recovery
    - TestRecovery_ShouldReturn500AndEmitOnPanic: a panic yields 500, API-011, and catalog conformance.
    - TestRecovery_ShouldPassThroughWhenNoPanic: normal responses are untouched.
    - TestRecovery_ShouldRepanicAbortHandler: http.ErrAbortHandler is re-panicked, not logged.
    - TestRecovery_ShouldNotOverwriteWrittenResponse: a panic after a write does not clobber the response.

Tested elsewhere:

Declined:

Additional Remarks:
  Installs the global emitter hook via the recorder, so these tests run
  sequentially.
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

func TestRecovery_ShouldReturn500AndEmitOnPanic(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	panicky := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	rw := httptest.NewRecorder()

	// ACT
	require.NotPanics(t, func() {
		Recovery(panicky).ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	})

	// ASSERT
	assert.Equal(t, http.StatusInternalServerError, rw.Code)
	rec.AssertEmitted(t, catalog.API011)
	rec.AssertData(t, catalog.API011, "panic", "boom")
	rec.AssertMatchesCatalog(t)
}

func TestRecovery_ShouldPassThroughWhenNoPanic(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	rw := httptest.NewRecorder()

	// ACT
	Recovery(ok).ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))

	// ASSERT
	assert.Equal(t, http.StatusNoContent, rw.Code)
	rec.AssertNotEmitted(t, catalog.API011)
}

func TestRecovery_ShouldRepanicAbortHandler(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	aborting := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	})
	rw := httptest.NewRecorder()

	// ACT / ASSERT — the sentinel propagates so net/http can abort silently,
	// and it is not turned into an API-011 error event.
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		Recovery(aborting).ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	})
	rec.AssertNotEmitted(t, catalog.API011)
}

func TestRecovery_ShouldNotOverwriteWrittenResponse(t *testing.T) { //nolint:paralleltest // installs the global emitter hook
	// ARRANGE — the handler commits a response, then panics.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))

		panic("boom after write")
	})
	rw := httptest.NewRecorder()

	// ACT
	require.NotPanics(t, func() {
		Recovery(handler).ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil))
	})

	// ASSERT — the committed response is left intact (no 500 body appended), and
	// the panic is still recorded.
	assert.Equal(t, http.StatusOK, rw.Code)
	assert.Equal(t, "partial", rw.Body.String())
	rec.AssertEmitted(t, catalog.API011)
}
