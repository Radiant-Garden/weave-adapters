/*
Testing: recorder.go

Pending:

Tested:
  NewRecorder / WriteHeader / Write / Status / Bytes / Wrote
    - TestRecorder_ShouldDefaultToStatusOK: a body without a status is a 200, as in net/http.
    - TestRecorder_ShouldRecordAnExplicitStatus: the status is captured and forwarded.
    - TestRecorder_ShouldKeepTheFirstStatus: a late second WriteHeader cannot rewrite history.
    - TestRecorder_ShouldAccumulateBytes: the byte count spans several writes.
    - TestRecorder_ShouldReportCommitted: Wrote flips on either a status or a body byte.
  Unwrap
    - TestRecorder_ShouldExposeTheUnderlyingWriter: ResponseController can still reach it.

Tested elsewhere:
  Its use by the logging and recovery middleware is covered in their own tests,
  which is where the semantics this type centralises were originally asserted.

Declined:

Additional Remarks:
  This type exists to stop four wrappers re-deriving the same bookkeeping, so
  the cases here are exactly the ones they each got slightly differently.
*/

package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_ShouldDefaultToStatusOK(t *testing.T) {
	t.Parallel()

	// ARRANGE
	rec := httptest.NewRecorder()
	r := NewRecorder(rec)

	// ACT — a handler that writes a body without setting a status.
	_, err := r.Write([]byte("body"))
	require.NoError(t, err)

	// ASSERT
	assert.Equal(t, http.StatusOK, r.Status())
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRecorder_ShouldRecordAnExplicitStatus(t *testing.T) {
	t.Parallel()

	// ARRANGE
	rec := httptest.NewRecorder()
	r := NewRecorder(rec)

	// ACT
	r.WriteHeader(http.StatusTeapot)

	// ASSERT — recorded for the caller and forwarded to the client.
	assert.Equal(t, http.StatusTeapot, r.Status())
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestRecorder_ShouldKeepTheFirstStatus(t *testing.T) {
	t.Parallel()

	// ARRANGE
	rec := httptest.NewRecorder()
	r := NewRecorder(rec)

	// ACT — net/http commits the first status; a second call is a bug that
	// would otherwise be recorded as if it had taken effect.
	r.WriteHeader(http.StatusCreated)
	r.WriteHeader(http.StatusNotFound)

	// ASSERT
	assert.Equal(t, http.StatusCreated, r.Status())
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestRecorder_ShouldAccumulateBytes(t *testing.T) {
	t.Parallel()

	// ARRANGE
	r := NewRecorder(httptest.NewRecorder())

	// ACT
	for _, chunk := range []string{"one", "two", "three"} {
		_, err := r.Write([]byte(chunk))
		require.NoError(t, err)
	}

	// ASSERT — the request-audit event reports this as bytesWritten.
	assert.Equal(t, len("one")+len("two")+len("three"), r.Bytes())
}

func TestRecorder_ShouldReportCommitted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		act  func(r *Recorder)
	}{
		{name: "should be committed by a status", act: func(r *Recorder) { r.WriteHeader(http.StatusOK) }},
		{name: "should be committed by a body byte", act: func(r *Recorder) { _, _ = r.Write([]byte("x")) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			r := NewRecorder(httptest.NewRecorder())
			require.False(t, r.Wrote())

			// ACT
			tt.act(r)

			// ASSERT — the recovery middleware keys on this to decide whether a
			// 500 would corrupt an already-started response.
			assert.True(t, r.Wrote())
		})
	}
}

func TestRecorder_ShouldExposeTheUnderlyingWriter(t *testing.T) {
	t.Parallel()

	// ARRANGE
	rec := httptest.NewRecorder()

	// ACT / ASSERT — Unwrap is how http.ResponseController reaches Flusher and
	// friends through the wrapper.
	assert.Equal(t, http.ResponseWriter(rec), NewRecorder(rec).Unwrap())
}
