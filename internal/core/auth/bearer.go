package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

const (
	authorizationHeader = "Authorization"
	bearerScheme        = "Bearer"

	// callerRole is the role recorded for an authenticated caller. Every caller
	// is a service today; a real role vocabulary arrives if weave ever issues
	// per-identity credentials.
	callerRole = "service"

	// maxLoggedSchemeLen bounds what a rejected scheme contributes to a log
	// line. The value is attacker-controlled, so it is truncated to keep a
	// flood of oversized headers from bloating the operator log.
	maxLoggedSchemeLen = 16
)

// Bearer returns middleware that authenticates requests against v.
//
// It must run inside RequestID (so events carry a request ID) and inside
// Logging (so a rejected request still produces its API-010 audit line), but
// outside any middleware that reads the body — an unauthenticated caller must
// not be able to make the adapter read one.
//
// Requests for which skip returns true bypass authentication entirely: health
// is unauthenticated by contract so weave can poll it, and the reserved spec
// route carries nothing worth protecting.
func Bearer(v *Verifier, skip func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip != nil && skip(r) {
				next.ServeHTTP(w, r)

				return
			}

			entry, err := authenticate(v, r)
			if err != nil {
				// One rejection, one event, one body: the error carries the
				// event that describes this specific failure, and WriteError
				// renders the response from that same catalog entry.
				apierror.WriteError(w, r, err)

				return
			}

			// The label becomes caller.subject on every event this request
			// emits — the answer to "who did this" in the audit log. It is set
			// in place so the logging middleware, which wraps this one, sees it
			// on the API-010 line it emits after the handler returns.
			if !events.SetIdentity(r.Context(), entry.Label, callerRole) {
				// No caller context to enrich: this route was mounted outside
				// the request-ID middleware. Attach one rather than dropping
				// the identity.
				r = r.WithContext(events.WithCaller(r.Context(), events.Caller{
					Subject:    entry.Label,
					Role:       callerRole,
					RemoteAddr: r.RemoteAddr,
					Method:     r.Method,
					Path:       r.URL.Path,
				}))
			}

			next.ServeHTTP(w, r)
		})
	}
}

// authenticate resolves the request's credential, returning an *apierror.Error
// bound to the event describing the failure.
func authenticate(v *Verifier, r *http.Request) (Entry, error) {
	header := r.Header.Get(authorizationHeader)
	if header == "" {
		return Entry{}, apierror.New(catalog.API020)
	}

	token, ok := bearerToken(header)
	if !ok {
		return Entry{}, apierror.New(catalog.API021, "scheme", loggedScheme(header))
	}

	entry, err := v.Verify(token)

	switch {
	case errors.Is(err, ErrTokenExpired):
		return Entry{}, apierror.New(catalog.API023,
			"label", entry.Label,
			"expiredAt", entry.ExpiresAt.Time().Format(time.RFC3339),
		)
	case err != nil:
		return Entry{}, apierror.New(catalog.API022)
	}

	return entry, nil
}

// bearerToken extracts the credential from an Authorization header value,
// reporting whether the header used the Bearer scheme.
//
// The scheme is compared case-insensitively (RFC 9110 makes it case-insensitive)
// but the token is taken verbatim — trimming or normalizing it would let two
// different strings authenticate as one token.
func bearerToken(header string) (token string, ok bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}

	// Only leading padding from the split is dropped; the credential itself is
	// never altered.
	token = strings.TrimLeft(rest, " ")
	if token == "" {
		return "", false
	}

	return token, true
}

// loggedScheme renders the rejected header for the operator log.
//
// A header with no space is reported as "(none)" rather than echoed. That case
// is a bare credential — the single most likely malformed header, because
// weave's credential store sends apiToken verbatim — so echoing the "scheme"
// would write a live token into the log. "(none)" is also the more useful
// diagnostic: it says the header had no scheme at all, which is the fix.
func loggedScheme(header string) string {
	scheme, _, found := strings.Cut(header, " ")
	if !found {
		return "(none)"
	}

	if len(scheme) > maxLoggedSchemeLen {
		return scheme[:maxLoggedSchemeLen] + "…"
	}

	return scheme
}
