/*
Testing: chain.go

Pending:

Tested:
  Chain
    - TestChain_ShouldApplyOutermostFirst: the first middleware wraps the rest.

Tested elsewhere:

Declined:

Additional Remarks:
*/

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChain_ShouldApplyOutermostFirst(t *testing.T) {
	t.Parallel()

	// ARRANGE — each middleware records its name as the request passes through.
	var order []string

	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)

				next.ServeHTTP(w, r)
			})
		}
	}

	final := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "handler")
	})

	// ACT
	h := Chain(final, tag("a"), tag("b"), tag("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	// ASSERT — first middleware runs first (outermost).
	assert.Equal(t, []string{"a", "b", "c", "handler"}, order)
}
