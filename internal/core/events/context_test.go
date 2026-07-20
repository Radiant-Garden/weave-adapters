/*
Testing: context.go

Pending:

Tested:
  WithCaller / CallerFrom / callerFrom
    - TestWithCaller_ShouldRoundTrip: values set via WithCaller are read back by CallerFrom.

Tested elsewhere:

Declined:

Additional Remarks:
*/

package events

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithCaller_ShouldRoundTrip(t *testing.T) {
	t.Parallel()

	// ARRANGE
	want := Caller{
		Subject: "svc", Role: "admin", RemoteAddr: "1.2.3.4",
		RequestID: "req-1", Method: "GET", Path: "/x",
	}

	// ACT
	got := CallerFrom(WithCaller(context.Background(), want))

	// ASSERT
	assert.Equal(t, want, got)
}
