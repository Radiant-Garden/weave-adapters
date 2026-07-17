/*
Testing: logging.go

Pending:

Tested:
  Logging (via RequestID → Logging chain)
    - TestLogging_ShouldEmitRequestCompleted: API-010 with status, bytes, and the
      caller/request groups; the recorded event conforms to the catalog.

Tested elsewhere:

Declined:

Additional Remarks:
  Installs the global emitter hook via the recorder, so this test runs
  sequentially. Logging is exercised behind RequestID because API-010 is an
  ExternalSource event and needs the caller context RequestID installs.
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
	h := Chain(handler, RequestID, Logging)
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

	// The emitted fields must match the catalog spec (no drift).
	rec.AssertMatchesCatalog(t)
}
