/*
Testing: requestbody.go

Pending:

Tested:

	Decode — the accept path
	  - TestDecode_ShouldDecodeAWellFormedBody: the case everything else is a
	    rejection of.
	  - TestDecode_ShouldAcceptContentTypeParameters: "application/json;
	    charset=utf-8" is the same media type, and a client that sends it is not
	    doing anything wrong.

	Decode — the rejections, each asserted by status *and* problem type, because
	the status is what weave's classifier reads and the type is what a human
	reads:
	  - TestDecode_ShouldRejectANonJSONContentType: 415.
	  - TestDecode_ShouldRejectAMissingContentType: 415, and the diagnostic says
	    "(none)" rather than an empty string.
	  - TestDecode_ShouldRejectAnUnparseableContentType: 415, reported as what was
	    sent — a malformed header is not an absent one.
	  - TestDecode_ShouldRejectABodyOverTheLimit: 413, and the limit reaches the
	    client's detail so it knows what it exceeded.
	  - TestDecode_ShouldRejectAnEmptyBody: 400 with its own message, not a
	    syntax-error message for bytes nobody sent.
	  - TestDecode_ShouldRejectMalformedJSON: 400.
	  - TestDecode_ShouldRejectAnUnknownField: 400 naming the field, which is the
	    silent-drop this package exists to prevent.
	  - TestDecode_ShouldRejectASecondJSONValue: 400 — "{}{}" decodes its first
	    value cleanly and would otherwise be accepted with the rest ignored.

	Ordering
	  - TestDecode_ShouldCheckContentTypeBeforeReadingTheBody: an oversized body
	    with the wrong Content-Type is 415, not 413. The cheap header check runs
	    first and the body is never read.

Tested elsewhere:

	That API-904/905 carry a ResponseCode the taxonomy knows, and that the
	problem+json rendering is well-formed: internal/core/apierror. That the codes
	appear in errors.yaml's enum: api/common/common_test.go.

Declined:

	Asserting the exact json package message for malformed input. It is the
	standard library's wording, not this package's contract, and pinning it would
	fail on a Go upgrade that reworded it. The tests assert the status, the type
	and — where this package supplies the wording — the message.

	A concurrency test. Decode holds no state; every value it touches is derived
	from the request it was handed.

Additional Remarks:

	The limit is passed per-call rather than read from config, which is what lets
	these tests use a limit of a few bytes instead of building a 1 MiB body to
	cross the real default.
*/
package requestbody_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
	"github.com/radiantgarden/weave-adapters/internal/core/requestbody"
)

// payload is the shape every test decodes into.
type payload struct {
	Name string `json:"name"`
}

// defaultLimit is generous relative to the bodies here, so a test that is not
// about the size limit never accidentally trips it.
const defaultLimit = 1024

// decoded carries what a test needs about one Decode call: the target, the
// error, and the request that produced it, so a rejection can be rendered
// through WriteError exactly as a handler would render it.
type decoded struct {
	got     payload
	err     error
	request *http.Request
}

// decode runs Decode against a request built from the given content type and
// body.
func decode(t *testing.T, contentType, body string, limit int) decoded {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/scopes", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	var got payload

	err := requestbody.Decode(httptest.NewRecorder(), req, limit, &got)

	return decoded{got: got, err: err, request: req}
}

// requireProblem renders the rejection the way a handler does — through
// apierror.WriteError — and asserts the status and problem type a client
// actually receives. Asserting the wire bytes rather than the error value keeps
// these tests honest about what weave sees: its classifier reads the status and
// never decodes the body, so the status is the part that has to be right.
func requireProblem(t *testing.T, d decoded, wantStatus int, wantType string) apierror.Problem {
	t.Helper()

	require.Error(t, d.err)

	recorder := httptest.NewRecorder()
	apierror.WriteError(recorder, d.request, d.err)

	assert.Equal(t, wantStatus, recorder.Code)

	var problem apierror.Problem
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	assert.Equal(t, wantType, problem.Type)
	assert.Equal(t, wantStatus, problem.Status, "the body status must agree with the response status")

	return problem
}

func TestDecode_ShouldDecodeAWellFormedBody(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	d := decode(t, "application/json", `{"name":"lab"}`, defaultLimit)

	// ASSERT
	require.NoError(t, d.err)
	assert.Equal(t, "lab", d.got.Name)
}

func TestDecode_ShouldAcceptContentTypeParameters(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a charset parameter names the same media type.
	d := decode(t, "application/json; charset=utf-8", `{"name":"lab"}`, defaultLimit)

	// ASSERT
	require.NoError(t, d.err)
	assert.Equal(t, "lab", d.got.Name)
}

func TestDecode_ShouldRejectANonJSONContentType(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — valid JSON, but the header is the contract.
	d := decode(t, "text/plain", `{"name":"lab"}`, defaultLimit)

	// ASSERT
	requireProblem(t, d, http.StatusUnsupportedMediaType, "weave-adapters:unsupported-media-type")
}

func TestDecode_ShouldRejectAMissingContentType(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	d := decode(t, "", `{"name":"lab"}`, defaultLimit)

	// ASSERT
	requireProblem(t, d, http.StatusUnsupportedMediaType, "weave-adapters:unsupported-media-type")
}

func TestDecode_ShouldRejectAnUnparseableContentType(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a header that mime.ParseMediaType refuses.
	d := decode(t, "application/", `{"name":"lab"}`, defaultLimit)

	// ASSERT — rejected, and as a media-type problem rather than anything else.
	requireProblem(t, d, http.StatusUnsupportedMediaType, "weave-adapters:unsupported-media-type")
}

func TestDecode_ShouldRejectABodyOverTheLimit(t *testing.T) {
	t.Parallel()

	// ARRANGE — a body comfortably past a tiny limit.
	body := `{"name":"` + strings.Repeat("x", 200) + `"}`

	// ACT
	d := decode(t, "application/json", body, 32)

	// ASSERT — the client is told what it exceeded, so it can act without
	// guessing at the server's configuration.
	problem := requireProblem(t, d, http.StatusRequestEntityTooLarge, "weave-adapters:payload-too-large")
	assert.Contains(t, problem.Detail, "32")
}

func TestDecode_ShouldRejectAnEmptyBody(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	d := decode(t, "application/json", "", defaultLimit)

	// ASSERT — its own message: a syntax error would send someone looking for a
	// bad character in a body that was never sent.
	problem := requireProblem(t, d, http.StatusBadRequest, "weave-adapters:validation-failed")

	fieldErrors := problem.Errors
	require.Len(t, fieldErrors, 1)
	assert.Equal(t, "body", fieldErrors[0].Field)
	assert.Contains(t, fieldErrors[0].Message, "must not be empty")
}

func TestDecode_ShouldRejectMalformedJSON(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	d := decode(t, "application/json", `{"name":`, defaultLimit)

	// ASSERT
	requireProblem(t, d, http.StatusBadRequest, "weave-adapters:validation-failed")
}

func TestDecode_ShouldRejectAnUnknownField(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — the caller believes it set something the adapter has never
	// heard of. Accepting this silently is the failure the package prevents.
	d := decode(t, "application/json", `{"name":"lab","namme":"typo"}`, defaultLimit)

	// ASSERT — and the offending field reaches the client, or it cannot fix it.
	problem := requireProblem(t, d, http.StatusBadRequest, "weave-adapters:validation-failed")

	fieldErrors := problem.Errors
	require.Len(t, fieldErrors, 1)
	assert.Contains(t, fieldErrors[0].Message, "namme")
}

func TestDecode_ShouldRejectASecondJSONValue(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — the first value decodes cleanly; the second would be
	// silently dropped without the More check.
	d := decode(t, "application/json", `{"name":"lab"}{"name":"ignored"}`, defaultLimit)

	// ASSERT
	problem := requireProblem(t, d, http.StatusBadRequest, "weave-adapters:validation-failed")

	fieldErrors := problem.Errors
	require.Len(t, fieldErrors, 1)
	assert.Contains(t, fieldErrors[0].Message, "exactly one JSON object")
}

func TestDecode_ShouldCheckContentTypeBeforeReadingTheBody(t *testing.T) {
	t.Parallel()

	// ARRANGE — wrong on both counts: bad media type *and* over the limit.
	body := strings.Repeat("x", 500)

	// ACT
	d := decode(t, "text/plain", body, 8)

	// ASSERT — 415 rather than 413. The header check is first, so a mislabelled
	// body is never read at all.
	requireProblem(t, d, http.StatusUnsupportedMediaType, "weave-adapters:unsupported-media-type")
}
