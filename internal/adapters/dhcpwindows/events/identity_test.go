/*
Testing: identity.go

Pending:

Tested:

	CategoryDHCP
	  - TestDHCPCategory_ShouldBeOwnedByTheAdapter: the prefix is declared here,
	    not in core — the distinction from the shared BACKEND category.

	init (DHCP registration)
	  - TestDHCPCatalog_ShouldRegisterTheIdentityEvent: DHCP-001 registers with the
	    adapter's own category, INFO level, and both identity inputs as required
	    string fields.
	  - TestDHCPCatalog_ShouldNotBeExternalSource: a startup event has no request
	    context to read, so Emit would panic.
	  - TestDHCPCatalog_ShouldNameTheFingerprintRatherThanTheKey: the field is a
	    fingerprint by name and description, so nobody later "improves" it by
	    logging the key itself.

Tested elsewhere:

	DHCP-001 emission at startup: cmd/weave-adapter-dhcp-windows's tests, which
	  drive the real wiring — registration lives here, emission is the binary's,
	  the same split as SYS-001.
	NamespaceKeyFingerprint's one-way property: the adapter's identity_test.go.

Declined:

	Asserting the Troubleshooting and Description prose, for the reason given in
	  backend_test.go: wording is not behaviour.

Additional Remarks:

	Registration happens in init(); the registry is read-only during the test, so
	this is parallel-safe.
*/
package events

import (
	"log/slog"
	"testing"

	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDHCPCategory_ShouldBeOwnedByTheAdapter(t *testing.T) {
	t.Parallel()

	// ASSERT — DHCP belongs to this adapter alone, so the constant lives here.
	// BACKEND is the deliberate exception: it is shared, so core declares it to
	// stop a second one being invented. Core has no reason to know this prefix
	// exists, and declaring it there would make the adapter-agnostic package
	// name a specific backend.
	assert.Equal(t, "DHCP", CategoryDHCP.String())
}

func TestDHCPCatalog_ShouldRegisterTheIdentityEvent(t *testing.T) {
	t.Parallel()

	e, ok := coreevents.Get(DHCP001)
	require.True(t, ok, "DHCP-001 should be registered")

	assert.Equal(t, CategoryDHCP.String(), e.Category)

	// INFO, not WARN: a resolved identity is the normal case. It becomes
	// actionable only by comparison with a previous start, which is a human
	// judgement rather than a severity.
	assert.Equal(t, slog.LevelInfo, e.Level)

	// Both inputs are required because the event's entire purpose is comparing
	// them against the previous start; one alone cannot tell a re-key from a
	// rename.
	require.Len(t, e.Fields, 2)
	assert.Equal(t, "serverName", e.Fields[0].Name)
	assert.True(t, e.Fields[0].Required)
	assert.Equal(t, "namespaceKeyFingerprint", e.Fields[1].Name)
	assert.True(t, e.Fields[1].Required)
}

func TestDHCPCatalog_ShouldNotBeExternalSource(t *testing.T) {
	t.Parallel()

	e, ok := coreevents.Get(DHCP001)
	require.True(t, ok)

	// Emitted during startup wiring, before any request exists. Emit panics on
	// an ExternalSource event whose context carries no remoteAddr, so this would
	// crash the adapter on every start.
	assert.False(t, e.ExternalSource, "a startup event has no request context to read")
}

func TestDHCPCatalog_ShouldNameTheFingerprintRatherThanTheKey(t *testing.T) {
	t.Parallel()

	e, ok := coreevents.Get(DHCP001)
	require.True(t, ok)

	// The catalog is where an operator learns what a field contains, and this
	// one is a hash of a backup-critical value that must never be logged. Both
	// the name and the description say so, so a later change that starts
	// emitting the key itself contradicts its own documentation here.
	assert.Contains(t, e.Fields[1].Name, "Fingerprint")
	assert.Contains(t, e.Fields[1].Description, "never the key itself")
	assert.NotContains(t, e.Example, "namespaceKey\":\"")
}
