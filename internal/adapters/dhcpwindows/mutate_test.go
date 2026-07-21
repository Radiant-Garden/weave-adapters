/*
Testing: mutate.go

Pending:

	DeleteScope and UpdateScope against a real WS2022 host. Everything here runs
	against the fake runner, so it proves what the adapter sends and how it reads
	the answer; it cannot prove Remove-/Set-DhcpServerv4Scope accept that
	parameter set, nor capture a real Set … -PassThru payload. The e2e gate and
	host sign-off cover those, the same gap create carries.

Tested:

	Client.DeleteScope
	  - TestDeleteScope_ShouldResolveThenRemove: two spawns — list to resolve, then remove on the resolved subnet.
	  - TestDeleteScope_ShouldReturnNotFoundForAnUnknownWadaptID: an absent identity is ErrScopeNotFound, and the remove never runs.
	  - TestDeleteScope_ShouldNeverInterpolateIntoTheScript: the injection property, on the write path.
	Client.UpdateScope
	  - TestUpdateScope_ShouldSetAndReturnTheUpdatedScope: two spawns, the updated scope decoded and its identity stable.
	  - TestUpdateScope_ShouldReturnNotFoundForAnUnknownWadaptID: absent is ErrScopeNotFound, and the set never runs.
	  - TestUpdateScope_ShouldRejectARangeThatLeavesTheSubnet: an out-of-subnet resize is ErrRangeOutsideSubnet before any set.
	  - TestUpdateScope_ShouldSplatBothRangeEndsForAOneSidedResize: a one-sided resize still passes both -StartRange and -EndRange.
	  - TestUpdateScope_ShouldRejectAScopeWhoseIdentityMoved: a -PassThru scope on a different subnet is a backend error, not a served resource.
	  - TestUpdateScope_ShouldRebaselineDriftSilently: an intentional PATCH emits no DHCP-002, while a later external change still does.

Tested elsewhere:

	ScopeUpdate.Validate, env and the range check: update_test.go. The handler's
	mapping of these outcomes to status codes: scope_item_test.go. ListScopes
	decoding and the derivation: client_test.go and identity_test.go.

Declined:

	Exercising the resolve/mutate race the two-spawn design cannot close. It needs
	two real concurrent operations on one scope; the fake cannot produce it, and
	the behaviour under it is documented on DeleteScope rather than claimed.

Additional Remarks:

	The fake runner's stdouts queue is what makes the two-spawn calls testable:
	update lists then sets, so the two spawns must see different output. A single
	stdout would make the resolve and the set read the same bytes.

	TestDeleteScope_ShouldNeverInterpolateIntoTheScript is the write-path twin of
	create's injection test — the one to keep if this file ever has to shrink.
*/
package dhcpwindows

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	eventstest "github.com/radiantgarden/weave-adapters/internal/core/events/testing"
)

// scopeListJSON is a one-scope listing on 10.0.30.0/24, parameterised by name so
// a drift test can vary the one fingerprint field it turns on.
func scopeListJSON(name string) string {
	return fmt.Sprintf(`[{"scopeId":"10.0.30.0","subnetMask":"255.255.255.0",`+
		`"startRange":"10.0.30.10","endRange":"10.0.30.250","name":%q,`+
		`"state":"Active","type":"Dhcp","leaseDurationSeconds":691200}]`, name)
}

// wadaptIDFor derives the identity the client under test would assign to a
// subnet, so a test can address the scope the same way production does.
func wadaptIDFor(c *Client, scopeID string) string {
	return deriveWadaptID(c.namespaceKey, c.serverName, scopeID)
}

func TestDeleteScope_ShouldResolveThenRemove(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fake := &fakeRunner{stdout: []byte(scopeListJSON("lab"))}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	err := client.DeleteScope(context.Background(), target)

	// ASSERT — two spawns: the list that resolves the identity, then the remove
	// on the resolved subnet, which reaches the script through the environment.
	require.NoError(t, err)
	require.Len(t, fake.scripts, 2)
	assert.Equal(t, listScopesScript, fake.scripts[0])
	assert.Equal(t, deleteScopeScript, fake.scripts[1])
	assert.Equal(t, "10.0.30.0", fake.envs[1][envScopeID])
}

func TestDeleteScope_ShouldReturnNotFoundForAnUnknownWadaptID(t *testing.T) {
	t.Parallel()

	// ARRANGE — the server holds one scope, and we ask to delete a different id.
	fake := &fakeRunner{stdout: []byte(scopeListJSON("lab"))}
	client := clientWith(fake)

	// ACT
	err := client.DeleteScope(context.Background(), "0000000000000")

	// ASSERT — ErrScopeNotFound, and the remove never ran: the resolve is the
	// only spawn, so nothing was deleted.
	require.ErrorIs(t, err, ErrScopeNotFound)
	assert.Len(t, fake.scripts, 1, "an unknown identity must not reach the remove")
}

func TestDeleteScope_ShouldNeverInterpolateIntoTheScript(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fake := &fakeRunner{stdout: []byte(scopeListJSON("lab"))}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	require.NoError(t, client.DeleteScope(context.Background(), target))

	// ASSERT — the remove script is the constant, and the subnet reached the
	// child through the environment rather than the command text.
	require.Len(t, fake.scripts, 2)
	assert.Equal(t, deleteScopeScript, fake.scripts[1],
		"the script must be the constant; nothing may be interpolated into it")
	assert.NotContains(t, fake.scripts[1], "10.0.30.0")
}

func TestUpdateScope_ShouldSetAndReturnTheUpdatedScope(t *testing.T) {
	t.Parallel()

	// ARRANGE — the list resolves the target; the set returns the changed scope.
	fake := &fakeRunner{stdouts: [][]byte{
		[]byte(scopeListJSON("lab")),
		[]byte(scopeListJSON("renamed")),
	}}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	updated, err := client.UpdateScope(context.Background(), target, ScopeUpdate{Name: new("renamed")})

	// ASSERT — the changed scope comes back with its identity unchanged, via two
	// spawns, the second carrying the new name through the environment.
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.Equal(t, target, updated.WadaptID, "the identity must not move under an update")

	require.Len(t, fake.scripts, 2)
	assert.Equal(t, listScopesScript, fake.scripts[0])
	assert.Equal(t, updateScopeScript, fake.scripts[1])
	assert.Equal(t, "renamed", fake.envs[1][envScopeName])
}

func TestUpdateScope_ShouldReturnNotFoundForAnUnknownWadaptID(t *testing.T) {
	t.Parallel()

	// ARRANGE
	fake := &fakeRunner{stdout: []byte(scopeListJSON("lab"))}
	client := clientWith(fake)

	// ACT
	_, err := client.UpdateScope(context.Background(), "0000000000000", ScopeUpdate{Name: new("x")})

	// ASSERT — ErrScopeNotFound, and the set never ran.
	require.ErrorIs(t, err, ErrScopeNotFound)
	assert.Len(t, fake.scripts, 1, "an unknown identity must not reach the set")
}

func TestUpdateScope_ShouldRejectARangeThatLeavesTheSubnet(t *testing.T) {
	t.Parallel()

	// ARRANGE — a new end one subnet over.
	fake := &fakeRunner{stdout: []byte(scopeListJSON("lab"))}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	_, err := client.UpdateScope(context.Background(), target, ScopeUpdate{EndRange: new("10.0.31.10")})

	// ASSERT — refused before the set, because a resize that leaves the subnet
	// would change the derived identity.
	require.ErrorIs(t, err, ErrRangeOutsideSubnet)
	assert.Len(t, fake.scripts, 1, "a rejected resize must not reach the set")
}

func TestUpdateScope_ShouldSplatBothRangeEndsForAOneSidedResize(t *testing.T) {
	t.Parallel()

	// ARRANGE — only the start changes, and it stays in-subnet.
	fake := &fakeRunner{stdouts: [][]byte{
		[]byte(scopeListJSON("lab")),
		[]byte(scopeListJSON("lab")),
	}}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	_, err := client.UpdateScope(context.Background(), target, ScopeUpdate{StartRange: new("10.0.30.20")})

	// ASSERT — the set was handed both range ends, the omitted one filled from the
	// existing scope, because the WithRange parameter set is mandatory-both.
	require.NoError(t, err)
	require.Len(t, fake.envs, 2)
	assert.Equal(t, "10.0.30.20", fake.envs[1][envScopeStartRange])
	assert.Equal(t, "10.0.30.250", fake.envs[1][envScopeEndRange])
}

func TestUpdateScope_ShouldRejectAScopeWhoseIdentityMoved(t *testing.T) {
	t.Parallel()

	// ARRANGE — the set returns a scope on a different subnet, which derives a
	// different identity than the one asked for.
	fake := &fakeRunner{stdouts: [][]byte{
		[]byte(scopeListJSON("lab")),
		[]byte(`[{"scopeId":"10.0.99.0","subnetMask":"255.255.255.0","startRange":"10.0.99.10",` +
			`"endRange":"10.0.99.250","name":"lab","state":"Active","type":"Dhcp","leaseDurationSeconds":691200}]`),
	}}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT
	_, err := client.UpdateScope(context.Background(), target, ScopeUpdate{Name: new("lab")})

	// ASSERT — refused rather than served: an update that produced a different
	// identity reached a different resource than the caller named.
	require.ErrorIs(t, err, ErrBackendMalformed)
}

//nolint:paralleltest // installs the global emitter hook
func TestUpdateScope_ShouldRebaselineDriftSilently(t *testing.T) {
	// ARRANGE — the drift ledger records the pre-update fingerprint during the
	// resolve, so without a re-baseline the next listing would see the intentional
	// change as drift.
	rec := eventstest.NewRecorder()
	t.Cleanup(rec.Install())

	fake := &fakeRunner{stdouts: [][]byte{
		[]byte(scopeListJSON("lab")),      // resolve, sets baseline
		[]byte(scopeListJSON("renamed")),  // set -PassThru, the intentional change
		[]byte(scopeListJSON("renamed")),  // a later poll: matches the re-baseline
		[]byte(scopeListJSON("hijacked")), // a later poll: a genuine external change
	}}
	client := clientWith(fake)
	target := wadaptIDFor(client, "10.0.30.0")

	// ACT / ASSERT — the update, then a poll returning the updated state.
	_, err := client.UpdateScope(context.Background(), target, ScopeUpdate{Name: new("renamed")})
	require.NoError(t, err)

	_, err = client.ListScopes(context.Background())
	require.NoError(t, err)

	// The intentional change did not fire DHCP-002: the re-baseline moved the
	// ledger to the new fingerprint, so the poll of the updated state saw no drift.
	rec.AssertNotEmitted(t, adapterevents.DHCP002)

	// ACT — a later poll where the scope has genuinely changed underneath us.
	_, err = client.ListScopes(context.Background())
	require.NoError(t, err)

	// ASSERT — now DHCP-002 fires, exactly once: the re-baseline updated the
	// fingerprint rather than discarding detection.
	rec.AssertEmittedN(t, adapterevents.DHCP002, 1)
}
