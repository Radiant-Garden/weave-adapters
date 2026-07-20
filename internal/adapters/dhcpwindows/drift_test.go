/*
Testing: drift.go

Pending:

	Observation against a real WS2022 host across an actual delete-and-recreate.
	Everything here drives the ledger with constructed scopes, which proves the
	comparison and the event; it cannot prove the recreated scope really does
	derive the same wadaptID on a live server. That is the sign-off criterion.

Tested:

	driftLedger.observe
	  - TestDrift_ShouldReportNothingOnTheFirstObservation: a first sighting is a
	    baseline, not drift.
	  - TestDrift_ShouldReportNothingWhenAttributesAreUnchanged: the steady state,
	    which is every poll — this firing would make the event useless.
	  - TestDrift_ShouldReportAReusedSubnet: the case the ledger exists for. Same
	    wadaptID, different scope behind it.
	  - TestDrift_ShouldReportEachChangedAttribute: name, ranges, mask and lease
	    each register.
	  - TestDrift_ShouldIgnoreDescriptionStateAndType: the three attributes that
	    change under ordinary operation must not warn, or an operator learns to
	    filter the event out.
	  - TestDrift_ShouldRememberADeletedScope: the entry survives the scope
	    vanishing from a listing, because that is precisely the entry needed to
	    notice the subnet being reused later.
	  - TestDrift_ShouldReBaselineAfterReporting: the same drift reports once, not
	    on every poll thereafter.
	  - TestDrift_ShouldBeUsableAsAZeroValue: *Client is built directly in tests,
	    so a ledger needing a constructor would be silently disabled there.
	  - TestDrift_ShouldTolerateConcurrentObservation: the health probe and a
	    request can observe at once.

	Client.reportDrift / changedFields
	  - TestReportDrift_ShouldEmitOneEventPerDriftedIdentity: two drifted scopes
	    are two events, because each is a separate reconciliation.
	  - TestReportDrift_ShouldNameTheChangedAttributes: the event says what moved.
	  - TestReportDrift_ShouldNotLogAttributeValues: names only. Scope names are
	    operator free text and ranges are topology; the field names are enough to
	    classify the change and the server holds the values.

Tested elsewhere:

	That DHCP-002 is registered with a level, fields and troubleshooting that
	satisfy the catalog's own rules: the events package's registration checks.
	wadaptID derivation: identity_test.go.

Declined:

	A test that the ledger is bounded. Growth is bounded by distinct subnets seen
	in one process lifetime, which no test can meaningfully assert without
	pinning an implementation detail; it is reasoned about in the type's doc
	instead.

	A test that a restart clears the ledger. That is what "in memory" means, and
	asserting it would test the Go runtime.

Additional Remarks:

	The three reportDrift tests are deliberately *not* parallel. Recorder.Install
	replaces a process-global event sink, so two of them running at once would
	uninstall each other's recorder and read each other's events. The ledger
	tests above are parallel because they never touch the sink.

	These drive the ledger directly rather than through a fake backend. The
	interesting behaviour is entirely in the comparison and what it remembers,
	and routing every case through a PowerShell fixture would obscure that
	without exercising anything the client tests do not already cover.
*/
package dhcpwindows

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	eventstesting "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
)

// scopeWith builds one identified scope, so a test can vary a single attribute
// and know the identity is held constant.
func scopeWith(wadaptID string, mutate func(*Scope)) Scope {
	scope := Scope{
		WadaptID:             wadaptID,
		ScopeID:              "10.0.30.0",
		SubnetMask:           "255.255.255.0",
		StartRange:           "10.0.30.10",
		EndRange:             "10.0.30.250",
		Name:                 "lab-vlan-30",
		State:                "Active",
		Type:                 "Dhcp",
		LeaseDurationSeconds: 691200,
	}

	if mutate != nil {
		mutate(&scope)
	}

	return scope
}

func TestDrift_ShouldReportNothingOnTheFirstObservation(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var ledger driftLedger

	// ACT
	reports := ledger.observe([]Scope{scopeWith("id-1", nil)})

	// ASSERT — nothing to compare against yet. Reporting here would fire on
	// every scope at every startup.
	assert.Empty(t, reports)
}

func TestDrift_ShouldReportNothingWhenAttributesAreUnchanged(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var ledger driftLedger

	scopes := []Scope{scopeWith("id-1", nil)}
	ledger.observe(scopes)

	// ACT — the steady state: weave polls, nothing has changed.
	reports := ledger.observe(scopes)

	// ASSERT
	assert.Empty(t, reports)
}

func TestDrift_ShouldReportAReusedSubnet(t *testing.T) {
	t.Parallel()

	// ARRANGE — the failure this exists to surface. The old scope is deleted and
	// a new one is created on the same subnet, so it derives the same wadaptID.
	var ledger driftLedger

	ledger.observe([]Scope{scopeWith("id-1", nil)})

	recreated := scopeWith("id-1", func(s *Scope) {
		s.Name = "finance-vlan-30"
		s.StartRange = "10.0.30.100"
		s.EndRange = "10.0.30.199"
	})

	// ACT
	reports := ledger.observe([]Scope{recreated})

	// ASSERT — one report, naming the identity weave holds.
	require.Len(t, reports, 1)
	assert.Equal(t, "id-1", reports[0].wadaptID)
	assert.Equal(t, "10.0.30.0", reports[0].scopeID)
	assert.Equal(t, "lab-vlan-30", reports[0].was.name)
	assert.Equal(t, "finance-vlan-30", reports[0].now.name)
}

func TestDrift_ShouldReportEachChangedAttribute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Scope)
	}{
		{"name", func(s *Scope) { s.Name = "renamed" }},
		{"startRange", func(s *Scope) { s.StartRange = "10.0.30.50" }},
		{"endRange", func(s *Scope) { s.EndRange = "10.0.30.200" }},
		{"subnetMask", func(s *Scope) { s.SubnetMask = "255.255.254.0" }},
		{"leaseDurationSeconds", func(s *Scope) { s.LeaseDurationSeconds = 86400 }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var ledger driftLedger

			ledger.observe([]Scope{scopeWith("id-1", nil)})

			// ACT
			reports := ledger.observe([]Scope{scopeWith("id-1", tc.mutate)})

			// ASSERT
			require.Len(t, reports, 1)
			assert.Equal(t, []string{tc.name}, changedFields(reports[0].was, reports[0].now))
		})
	}
}

func TestDrift_ShouldIgnoreDescriptionStateAndType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Scope)
	}{
		// The identity does not live in the description — that is the whole
		// point of deriving it — so editing one changes nothing.
		{"description", func(s *Scope) { s.Description = "edited by an operator" }},
		// Deactivating a scope is routine operation of the same scope.
		{"state", func(s *Scope) { s.State = "Inactive" }},
		{"type", func(s *Scope) { s.Type = "Both" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			var ledger driftLedger

			ledger.observe([]Scope{scopeWith("id-1", nil)})

			// ACT
			reports := ledger.observe([]Scope{scopeWith("id-1", tc.mutate)})

			// ASSERT — silence. An event that fires on ordinary administration
			// is one an operator learns to filter, which costs the cases that
			// matter.
			assert.Empty(t, reports)
		})
	}
}

func TestDrift_ShouldRememberADeletedScope(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var ledger driftLedger

	ledger.observe([]Scope{scopeWith("id-1", nil)})

	// The scope is deleted: it simply stops appearing in the listing.
	ledger.observe([]Scope{})

	// ACT — later, a new scope is created on the same subnet.
	reports := ledger.observe([]Scope{scopeWith("id-1", func(s *Scope) { s.Name = "reused" })})

	// ASSERT — the entry had to survive the absence, or the reuse this exists to
	// catch would look like a first sighting and report nothing.
	require.Len(t, reports, 1)
	assert.Equal(t, "lab-vlan-30", reports[0].was.name)
}

func TestDrift_ShouldReBaselineAfterReporting(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var ledger driftLedger

	ledger.observe([]Scope{scopeWith("id-1", nil)})

	changed := scopeWith("id-1", func(s *Scope) { s.Name = "renamed" })
	require.Len(t, ledger.observe([]Scope{changed}), 1)

	// ACT — the next poll sees the same, now-current, state.
	reports := ledger.observe([]Scope{changed})

	// ASSERT — reported once, not on every poll for the life of the process.
	assert.Empty(t, reports)
}

func TestDrift_ShouldBeUsableAsAZeroValue(t *testing.T) {
	t.Parallel()

	// ARRANGE — a Client built directly, as the tests in this package do.
	client := &Client{serverName: "dhcp01.example.test", namespaceKey: []byte(testNamespaceKey)}

	scopes := []Scope{scopeWith("id-1", nil)}

	// ACT / ASSERT — no constructor ran, and the ledger still records. A ledger
	// that needed one would be nil here and silently detect nothing.
	require.NotPanics(t, func() { client.drift.observe(scopes) })
	assert.Len(t, client.drift.observe([]Scope{scopeWith("id-1", func(s *Scope) { s.Name = "x" })}), 1)
}

func TestDrift_ShouldTolerateConcurrentObservation(t *testing.T) {
	t.Parallel()

	// ARRANGE — the health probe and a request can be in flight together.
	var ledger driftLedger

	scopes := []Scope{scopeWith("id-1", nil), scopeWith("id-2", nil)}

	// ACT / ASSERT — under -race, an unsynchronized map would fail here.
	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			ledger.observe(scopes)
		})
	}

	wg.Wait()
}

//nolint:paralleltest // Recorder.Install swaps a process-global sink; parallel runs uninstall each other.
func TestReportDrift_ShouldEmitOneEventPerDriftedIdentity(t *testing.T) {
	// ARRANGE
	recorder := eventstesting.NewRecorder()
	t.Cleanup(recorder.Install())

	client := &Client{serverName: "dhcp01.example.test", namespaceKey: []byte(testNamespaceKey)}

	before := []Scope{scopeWith("id-1", nil), scopeWith("id-2", nil)}
	client.reportDrift(t.Context(), before)

	after := []Scope{
		scopeWith("id-1", func(s *Scope) { s.Name = "a" }),
		scopeWith("id-2", func(s *Scope) { s.Name = "b" }),
	}

	// ACT
	client.reportDrift(t.Context(), after)

	// ASSERT — two events, not one summarizing both. Each names an identity an
	// operator has to reconcile on its own.
	emitted := recorder.FindByID(adapterevents.DHCP002)
	assert.Len(t, emitted, 2)
}

//nolint:paralleltest // Recorder.Install swaps a process-global sink; parallel runs uninstall each other.
func TestReportDrift_ShouldNameTheChangedAttributes(t *testing.T) {
	// ARRANGE
	recorder := eventstesting.NewRecorder()
	t.Cleanup(recorder.Install())

	client := &Client{serverName: "dhcp01.example.test", namespaceKey: []byte(testNamespaceKey)}

	client.reportDrift(t.Context(), []Scope{scopeWith("id-1", nil)})

	// ACT
	client.reportDrift(t.Context(), []Scope{scopeWith("id-1", func(s *Scope) {
		s.Name = "finance-vlan-30"
		s.StartRange = "10.0.30.100"
	})})

	// ASSERT
	emitted := recorder.FindByID(adapterevents.DHCP002)
	require.Len(t, emitted, 1)

	assert.Equal(t, "id-1", emitted[0].Data("wadaptId"))
	assert.Equal(t, "10.0.30.0", emitted[0].Data("scopeId"))
	assert.Equal(t, "name, startRange", emitted[0].Data("changed"))
}

//nolint:paralleltest // Recorder.Install swaps a process-global sink; parallel runs uninstall each other.
func TestReportDrift_ShouldNotLogAttributeValues(t *testing.T) {
	// ARRANGE
	recorder := eventstesting.NewRecorder()
	t.Cleanup(recorder.Install())

	client := &Client{serverName: "dhcp01.example.test", namespaceKey: []byte(testNamespaceKey)}

	const secretish = "tenant-acme-billing-vlan"

	client.reportDrift(t.Context(), []Scope{scopeWith("id-1", nil)})

	// ACT
	client.reportDrift(t.Context(), []Scope{scopeWith("id-1", func(s *Scope) { s.Name = secretish })})

	// ASSERT — the field names, never the values. A scope name is operator free
	// text and the ranges are topology; naming the fields is enough to classify
	// the change, and the DHCP server's own history holds the values.
	emitted := recorder.FindByID(adapterevents.DHCP002)
	require.Len(t, emitted, 1)

	for _, key := range []string{"wadaptId", "scopeId", "changed"} {
		assert.NotContains(t, fmt.Sprint(emitted[0].Data(key)), secretish)
	}
}
