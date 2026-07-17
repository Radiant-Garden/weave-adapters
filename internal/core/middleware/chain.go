// Package middleware provides the adapter's standard HTTP middleware chain:
// recovery, request-ID, and request logging, applied outermost-first as
// recovery → request-ID → logging. Auth and body limits arrive in M2; the
// metrics middleware is deferred with the rest of Prometheus.
package middleware

import "net/http"

// requestIDHeader is the inbound/outbound correlation header.
const requestIDHeader = "X-Request-Id"

// Middleware wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with the given middleware. The first middleware is the
// outermost layer — it runs first on the way in and last on the way out.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}

	return h
}
