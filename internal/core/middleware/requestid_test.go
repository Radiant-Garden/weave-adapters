/*
Testing: requestid.go

Pending:

Tested:
  RequestID
    - TestRequestID_ShouldGenerateWhenAbsent: a UUID is generated and echoed.
    - TestRequestID_ShouldPassThroughWhenPresent: an inbound ID is preserved.
    - TestRequestID_ShouldReplaceAnUnusableInboundID: oversized or control-byte IDs are regenerated, not echoed.
    - TestRequestID_ShouldBoundThePathItPutsInTheCallerContext: the path is truncated once, at the entry point.
  usableRequestID
    - Covered through TestRequestID_ShouldReplaceAnUnusableInboundID.
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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
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

func TestRequestID_ShouldReplaceAnUnusableInboundID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		inbound      string
		whyItMatters string
	}{
		{
			name:         "should replace an oversized ID",
			inbound:      strings.Repeat("x", maxInboundRequestIDLen+1),
			whyItMatters: "the ID is echoed into a header, the problem body, and every event line",
		},
		{
			name:         "should replace an ID carrying control bytes",
			inbound:      "abc\x00\x1bdef",
			whyItMatters: "a correlation key lands in log storage; control bytes there are somebody's problem",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.Header.Set("X-Request-Id", tt.inbound)

			rw := httptest.NewRecorder()

			// ACT
			h.ServeHTTP(rw, req)

			// ASSERT — replaced, not truncated: a truncated ID stops correlating
			// with the caller's own logs while still looking like theirs.
			assert.Regexp(t, uuidV4, rw.Header().Get("X-Request-Id"), tt.whyItMatters)
		})
	}
}

func TestRequestID_ShouldBoundThePathItPutsInTheCallerContext(t *testing.T) {
	t.Parallel()

	// ARRANGE — the path reaches every event line this request emits.
	var observed events.Caller

	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		observed = events.CallerFrom(r.Context())
	}))

	longPath := "/api/v1/" + strings.Repeat("a", 4000)
	rw := httptest.NewRecorder()

	// ACT
	h.ServeHTTP(rw, httptest.NewRequestWithContext(t.Context(), http.MethodGet, longPath, nil))

	// ASSERT
	assert.LessOrEqual(t, len(observed.Path), apierror.MaxReflectedPathLen+len("…"))
	assert.Contains(t, observed.Path, "…")
}

func TestNewRequestID_ShouldBeUniqueUUIDv4(t *testing.T) {
	t.Parallel()

	a := newRequestID()
	b := newRequestID()

	assert.Regexp(t, uuidV4, a)
	assert.NotEqual(t, a, b)
}
