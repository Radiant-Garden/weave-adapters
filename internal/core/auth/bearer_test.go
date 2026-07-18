/*
Testing: bearer.go

Pending:

Tested:
  Bearer
    - TestBearer_ShouldAllowAValidToken: the request reaches the handler.
    - TestBearer_ShouldPopulateCallerSubjectFromLabel: events answer "who did this".
    - TestBearer_ShouldExposeSubjectToOuterMiddleware: the request-audit line sees the identity too.
    - TestBearer_ShouldRejectEachFailureModeWithItsOwnEvent: four modes, four events, all 401.
    - TestBearer_ShouldNotDistinguishExpiredFromUnknown: no validity oracle.
    - TestBearer_ShouldSkipUnauthenticatedPaths: health stays open.
    - TestBearer_ShouldAcceptSchemeCaseInsensitively: RFC 9110 says the scheme is case-insensitive.
    - TestBearer_ShouldNeverLogOrReturnTheToken: the credential appears in no event and no body.
  bearerToken
    - TestBearerToken_ShouldParseHeaderForms: table over valid and malformed headers.
    - FuzzBearerToken: attacker-controlled input never panics.
  loggedScheme
    - TestLoggedScheme_ShouldNotEchoABareCredential: the likeliest malformed header is not logged.

Tested elsewhere:
  Token classification (unknown vs expired) is covered in verifier_test.go.

Declined:

Additional Remarks:
  Tests install the event recorder, which mutates the process-global emitter
  hook, so they run sequentially.

  Requests carry a caller context, matching production where RequestID runs
  before auth. WriteError seeds one when absent, which write_test.go covers.
*/

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
)

// fixtureValue is the accepted value in these tests. It is deliberately named
// and shaped to avoid gosec's hardcoded-credential heuristic — it is a fixture,
// not a secret.
const fixtureValue = "wadapt_aaa-bbb-ccc"

// serve runs a request through Bearer and returns the recorder. reached reports
// whether the wrapped handler ran.
func serve(t *testing.T, v *Verifier, header, path string) (rec *httptest.ResponseRecorder, reached bool) {
	t.Helper()

	handler := Bearer(v, func(r *http.Request) bool { return r.URL.Path == "/api/v1/health" })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true

			w.WriteHeader(http.StatusNoContent)
		}),
	)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req = req.WithContext(events.WithCaller(req.Context(), events.Caller{
		RemoteAddr: "192.0.2.1:1234",
		RequestID:  "req-abc",
		Method:     http.MethodGet,
		Path:       path,
	}))

	if header != "" {
		req.Header.Set(authorizationHeader, header)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec, reached
}

// configured returns a verifier holding fixtureValue under the label weave-prod.
func configured() *Verifier {
	return newVerifier(entryFor("weave-prod", fixtureValue, nil))
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldAllowAValidToken(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT
	resp, reached := serve(t, configured(), "Bearer "+fixtureValue, "/api/v1/leases")

	// ASSERT — and success is not its own event: API-010 already records every
	// completed request with the caller attached.
	assert.True(t, reached, "a valid token should reach the handler")
	assert.Equal(t, http.StatusNoContent, resp.Code)
	assert.Empty(t, rec.All(), "authenticating successfully should emit nothing")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldPopulateCallerSubjectFromLabel(t *testing.T) {
	// ARRANGE — capture the caller context the handler observes.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	var seen events.Caller

	handler := Bearer(configured(), nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = events.CallerFrom(r.Context())
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/leases", nil)
	req = req.WithContext(events.WithCaller(req.Context(), events.Caller{RemoteAddr: "192.0.2.1:1234"}))
	req.Header.Set(authorizationHeader, "Bearer "+fixtureValue)

	// ACT
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// ASSERT — the label is what makes an adapter's audit log answer "who".
	assert.Equal(t, "weave-prod", seen.Subject)
	assert.Equal(t, callerRole, seen.Role)
	assert.Equal(t, "192.0.2.1:1234", seen.RemoteAddr, "auth must not clobber the request-ID caller fields")
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldExposeSubjectToOuterMiddleware(t *testing.T) {
	// ARRANGE — logging wraps auth in production, and emits after the handler
	// returns. It must see the identity auth established beneath it, or the
	// request audit line carries an empty subject and the label buys nothing.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	var observedByOuter events.Caller

	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			// Read from the request this middleware holds — not the enriched
			// one the inner handler received.
			observedByOuter = events.CallerFrom(r.Context())
		})
	}

	handler := outer(Bearer(configured(), nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/leases", nil)
	req = req.WithContext(events.WithCaller(req.Context(), events.Caller{RemoteAddr: "192.0.2.1:1234"}))
	req.Header.Set(authorizationHeader, "Bearer "+fixtureValue)

	// ACT
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// ASSERT
	assert.Equal(t, "weave-prod", observedByOuter.Subject)
	assert.Equal(t, callerRole, observedByOuter.Role)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldRejectEachFailureModeWithItsOwnEvent(t *testing.T) {
	expired := entryFor("weave-expired", "wadapt_expired", NewExpiry(testClock.Add(-time.Hour)))

	tests := []struct {
		name      string
		header    string
		wantEvent events.EventID
	}{
		{name: "should reject a missing header", header: "", wantEvent: catalog.API020},
		{name: "should reject a non-bearer scheme", header: "Basic dXNlcjpwYXNz", wantEvent: catalog.API021},
		{name: "should reject a bare token with no scheme", header: fixtureValue, wantEvent: catalog.API021},
		{name: "should reject an unknown token", header: "Bearer wadapt_nope", wantEvent: catalog.API022},
		{name: "should reject an expired token", header: "Bearer wadapt_expired", wantEvent: catalog.API023},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			v := newVerifier(entryFor("weave-prod", fixtureValue, nil), expired)

			// ACT
			resp, reached := serve(t, v, tt.header, "/api/v1/leases")

			// ASSERT — every mode is a 401 problem+json with its own event...
			assert.False(t, reached, "a rejected request must not reach the handler")
			require.Equal(t, http.StatusUnauthorized, resp.Code)
			assert.Equal(t, "application/problem+json", resp.Header().Get("Content-Type"))

			rec.AssertEmittedN(t, tt.wantEvent, 1)
			rec.AssertMatchesCatalog(t)

			// ...and exactly one event, not one per layer.
			assert.Len(t, rec.All(), 1)
		})
	}
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldNotDistinguishExpiredFromUnknown(t *testing.T) {
	// ARRANGE — an expired token and a token that was never configured.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	v := newVerifier(entryFor("weave-expired", "wadapt_expired", NewExpiry(testClock.Add(-time.Hour))))

	// ACT
	expiredResp, _ := serve(t, v, "Bearer wadapt_expired", "/api/v1/leases")
	unknownResp, _ := serve(t, v, "Bearer wadapt_never-existed", "/api/v1/leases")

	// ASSERT — byte-identical. Any difference would confirm to an attacker that
	// a guessed token exists, turning the endpoint into a validity oracle.
	assert.Equal(t, expiredResp.Code, unknownResp.Code)
	assert.JSONEq(t, expiredResp.Body.String(), unknownResp.Body.String())

	// The operator still gets the distinction, in the log.
	rec.AssertEmitted(t, catalog.API023)
	rec.AssertData(t, catalog.API023, "label", "weave-expired")
	rec.AssertEmitted(t, catalog.API022)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldSkipUnauthenticatedPaths(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT — no credential at all, on the skip-listed path.
	resp, reached := serve(t, configured(), "", "/api/v1/health")

	// ASSERT — weave polls health to decide whether the adapter is reachable;
	// a 401 there would read as an outage.
	assert.True(t, reached)
	assert.Equal(t, http.StatusNoContent, resp.Code)
	assert.Empty(t, rec.All())
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldAcceptSchemeCaseInsensitively(t *testing.T) {
	// ARRANGE
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	// ACT — RFC 9110 makes the auth scheme case-insensitive.
	_, reached := serve(t, configured(), "bearer "+fixtureValue, "/api/v1/leases")

	// ASSERT
	assert.True(t, reached)
}

//nolint:paralleltest // installs the recorder, which mutates the global emitter hook
func TestBearer_ShouldNeverLogOrReturnTheToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "should not leak a valid token", header: "Bearer " + fixtureValue},
		{name: "should not leak an unknown token", header: "Bearer wadapt_some-unknown-secret"},
		{name: "should not leak a bare token sent with no scheme", header: "wadapt_bare-secret-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { //nolint:paralleltest // shares the global emitter hook
			// ARRANGE
			rec := eventstest.NewRecorder()
			t.Cleanup(rec.Install())

			// ACT
			resp, _ := serve(t, configured(), tt.header, "/api/v1/leases")

			// ASSERT — the credential must appear in no response body...
			credential := strings.TrimPrefix(tt.header, "Bearer ")
			assert.NotContains(t, resp.Body.String(), credential)

			// ...and in no recorded event field, at any level.
			for _, event := range rec.All() {
				encoded, err := json.Marshal(event.Groups)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), credential,
					"event %s carried the credential", event.ID)
			}
		})
	}
}

func TestBearerToken_ShouldParseHeaderForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{name: "should accept a well-formed header", header: "Bearer abc123", wantToken: "abc123", wantOK: true},
		{name: "should accept a lowercase scheme", header: "bearer abc123", wantToken: "abc123", wantOK: true},
		{name: "should accept extra padding after the scheme", header: "Bearer   abc123", wantToken: "abc123", wantOK: true},
		{name: "should preserve a token containing spaces", header: "Bearer abc 123", wantToken: "abc 123", wantOK: true},
		{name: "should reject another scheme", header: "Basic abc123", wantOK: false},
		{name: "should reject a bare token", header: "abc123", wantOK: false},
		{name: "should reject the scheme alone", header: "Bearer", wantOK: false},
		{name: "should reject an empty credential", header: "Bearer ", wantOK: false},
		{name: "should reject an empty header", header: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			token, ok := bearerToken(tt.header)

			// ASSERT
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantToken, token)
		})
	}
}

func TestLoggedScheme_ShouldNotEchoABareCredential(t *testing.T) {
	t.Parallel()

	// ARRANGE — a bare token is the likeliest malformed header, because weave's
	// credential store sends apiToken verbatim without a scheme.
	const bare = "wadapt_this-is-a-live-credential"

	// ACT
	got := loggedScheme(bare)

	// ASSERT — echoing the "scheme" here would write a live token to the log.
	assert.Equal(t, "(none)", got)
	assert.NotContains(t, got, "wadapt")

	// A real scheme is safe to log, and long ones are truncated.
	assert.Equal(t, "Basic", loggedScheme("Basic dXNlcjpwYXNz"))
	assert.LessOrEqual(t, len(loggedScheme(strings.Repeat("A", 200)+" x")), maxLoggedSchemeLen+len("…"))
}

// FuzzBearerToken drives the parser with arbitrary header bytes: the value is
// attacker-controlled, so it must never panic and must never report success
// without a credential to return.
func FuzzBearerToken(f *testing.F) {
	for _, seed := range []string{"", "Bearer ", "Bearer abc", "bearer  abc", "Basic x", "abc", "Bearer\x00abc", " "} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, header string) {
		token, ok := bearerToken(header)

		if ok && token == "" {
			t.Fatalf("bearerToken(%q) reported success with an empty token", header)
		}
	})
}
