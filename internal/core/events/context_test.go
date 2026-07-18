/*
Testing: context.go

Pending:

Tested:
  WithCaller / CallerFrom / callerFrom
    - TestWithCaller_ShouldRoundTrip: values set via WithCaller are read back by CallerFrom.
  InternalActorCtx
    - TestInternalActorCtx_ShouldSatisfyExternalSourceContract: emitting an
      ExternalSource event with it does not panic; role is "system".

Tested elsewhere:

Declined:

Additional Remarks:
*/

package events

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestInternalActorCtx_ShouldSatisfyExternalSourceContract(t *testing.T) { //nolint:paralleltest // mutates global registry
	withCleanRegistry(t)
	Register(&Event{ID: "TEST-020", Level: slog.LevelInfo, ExternalSource: true, Fields: callerFields()})

	ctx := InternalActorCtx("sweeper", "req-9")

	require.NotPanics(t, func() {
		Emit(ctx, "TEST-020")
	})
	assert.Equal(t, "system", callerFrom(ctx).Role)
	assert.Equal(t, "internal", callerFrom(ctx).RemoteAddr)
}
