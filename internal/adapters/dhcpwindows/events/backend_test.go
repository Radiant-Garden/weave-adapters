/*
Testing: backend.go

Pending:

Tested:

	init (BACKEND registration)
	  - TestBackendCatalog_ShouldRegisterTheCallFailureEvent: BACKEND-101 registers
	    with the shared Backend category, ERROR level, and operation/error as
	    required string fields.
	  - TestBackendCatalog_ShouldNotBeExternalSource: the guard that keeps the
	    health probe from panicking on its first run.
	  - TestBackendCatalog_ShouldSitInThisAdaptersPartition: the ID range that lets
	    a second adapter share the category without breaking single ownership.

Tested elsewhere:

	BACKEND-101 emission and its operation labels: the adapter's client and probe
	  tests (probe_test.go asserts it is emitted, with the right operation, and
	  that the payload matches the catalog).
	That every registered BACKEND event is in a partitioned range regardless of
	  which package registered it: layering_test.go, which checks the whole
	  registry rather than this one ID.

Declined:

	Asserting the Troubleshooting and Description text. Wording changes are not
	  behaviour, and pinning prose here would make every clarification a test
	  failure. What matters and is asserted is the machine-readable contract:
	  category, level, ExternalSource, fields, and the ID's partition.

Additional Remarks:

	Registration happens in init(); the registry is read-only during the test, so
	this is parallel-safe. Mirrors internal/core/events/catalog's tests, which is
	the precedent for testing a catalog file.
*/
package events

import (
	"log/slog"
	"testing"

	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendCatalog_ShouldRegisterTheCallFailureEvent(t *testing.T) {
	t.Parallel()

	e, ok := coreevents.Get(BACKEND101)
	require.True(t, ok, "BACKEND-101 should be registered")

	// The category constant is core's and shared; the event is this adapter's.
	assert.Equal(t, coreevents.CategoryBackend.String(), e.Category)
	assert.Equal(t, slog.LevelError, e.Level)

	// A failure is unactionable without knowing which call failed and why, so
	// both are required.
	assert.Equal(
		t,
		[]coreevents.FieldDef{
			{
				Name: "operation", Type: "string", Required: true,
				Description: "Which backend call failed (listScopes, createScope, updateScope, deleteScope, probe).",
			},
			{
				Name: "error", Type: "string", Required: true,
				Description: "The failure, including the shell's own stderr where it produced any.",
			},
		},
		e.Fields,
	)
}

func TestBackendCatalog_ShouldNotBeExternalSource(t *testing.T) {
	t.Parallel()

	e, ok := coreevents.Get(BACKEND101)
	require.True(t, ok)

	// Not a preference — a guard. The same call fails from a request-scoped
	// handler and from the health probe's background context, and Emit panics on
	// an ExternalSource event whose context carries no remoteAddr. Marking this
	// true would crash the adapter on its first health poll, which is also the
	// first time anything emits it.
	assert.False(t, e.ExternalSource,
		"a backend call describes the backend, not the inbound request that may have triggered it")
}

func TestBackendCatalog_ShouldSitInThisAdaptersPartition(t *testing.T) {
	t.Parallel()

	// The 1xx range is this adapter's; the next adapter takes 2xx. Sharing the
	// category while partitioning the IDs is what satisfies the single-owner
	// rule — a BACKEND-001 in the core catalog would break it as soon as two
	// adapters both wanted to emit a backend failure.
	assert.Regexp(t, `^BACKEND-1\d\d$`, string(BACKEND101))
}
