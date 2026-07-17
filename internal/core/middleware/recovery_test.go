/*
Testing: recovery.go

Pending:

Tested:
  Recovery
    - TestRecovery_ShouldReturn500AndEmitOnPanic: a panic yields 500 and API-011.
    - TestRecovery_ShouldPassThroughWhenNoPanic: normal responses are untouched.

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
