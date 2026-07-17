/*
Testing: requestid.go

Pending:

Tested:
  RequestID
    - TestRequestID_ShouldGenerateWhenAbsent: a UUID is generated and echoed.
    - TestRequestID_ShouldPassThroughWhenPresent: an inbound ID is preserved.
  newRequestID
    - TestNewRequestID_ShouldBeUniqueUUIDv4: format and uniqueness.

Tested elsewhere:
  Context population (caller/request): TestLogging_ShouldEmitRequestCompleted
    asserts the request group carries the generated requestId/method.

Declined:

Additional Remarks:
*/

package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRequestID_ShouldGenerateWhenAbsent(t *testing.T) {
	t.Parallel()

	// ARRANGE
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	rw := httptest.NewRecorder()

	// ACT
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	// ASSERT
	assert.Regexp(t, uuidV4, rw.Header().Get("X-Request-Id"))
}

func TestRequestID_ShouldPassThroughWhenPresent(t *testing.T) {
	t.Parallel()

	// ARRANGE
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "caller-supplied-id")

	rw := httptest.NewRecorder()

	// ACT
	h.ServeHTTP(rw, req)

	// ASSERT
	assert.Equal(t, "caller-supplied-id", rw.Header().Get("X-Request-Id"))
}

func TestNewRequestID_ShouldBeUniqueUUIDv4(t *testing.T) {
	t.Parallel()

	a := newRequestID()
	b := newRequestID()

	assert.Regexp(t, uuidV4, a)
	assert.NotEqual(t, a, b)
}
