/*
Testing: update.go

Pending:

	An update against a real WS2022 host. Everything here runs against the model
	and the fake runner, so it proves what the adapter validates and sends; it
	cannot prove Set-DhcpServerv4Scope accepts that parameter set, nor that a
	one-sided range change actually needs both -StartRange and -EndRange. The e2e
	gate and host sign-off cover those.

Tested:

	ScopeUpdate.Validate
	  - TestScopeUpdate_ShouldAcceptAWellFormedUpdate: every field set to a valid value reports nothing.
	  - TestScopeUpdate_ShouldAcceptAnEmptyUpdate: all fields absent is valid — a no-op merge.
	  - TestScopeUpdate_ShouldRejectEachMalformedField: every rule, one case each, including the > 0 lease bound.
	  - TestScopeUpdate_ShouldRejectAPresentButEmptyFreeTextField: empty name/description is a 400, not a clear.
	  - TestScopeUpdate_ShouldReportEveryFailureAtOnce: a wholly invalid update names all its fields.
	ScopeUpdate.env
	  - TestScopeUpdateEnv_ShouldSplatOnlyProvidedFields: an absent field is not in the environment.
	  - TestScopeUpdateEnv_ShouldSplatBothRangeEndsWhenEitherIsProvided: the WithRange set is mandatory-both, so a one-sided resize fills the other end from the existing scope.
	ScopeUpdate.validateEffectiveRange
	  - TestScopeUpdate_ShouldAcceptAResizeWithinTheSubnet: a range that stays in the subnet moves no identity.
	  - TestScopeUpdate_ShouldNameARangeFieldThatLeavesTheSubnet: the offending end is reported, using the existing counterpart for the side left out.
	  - TestScopeUpdate_ShouldNameARangeEndOnTheNetworkOrBroadcastAddress: inside the subnet and still not leasable — the same rule create enforces, through the same error.
	  - TestScopeUpdate_ShouldNotJudgeLeasabilityOfAnUnparseableEnd: a value that does not parse is named once, for not parsing, and the leasable check does not touch the zero address.
	  - TestScopeUpdate_ShouldRejectAOneSidedResizeThatInvertsTheRange: the bug this function exists to prevent, on both sides.
	  - TestScopeUpdate_ShouldNameAFieldOnceWhenItBreaksTwoRules: an end outside the subnet is usually inverted as well.

Tested elsewhere:

	DeleteScope and UpdateScope end to end against the fake runner, including the
	drift re-baseline: mutate_test.go. The handler's mapping of these to status
	codes: scope_item_test.go. parseIPv4/networkOf/hasControlChars/fieldError are
	shared with create and tested there.

Declined:

	Asserting the exact wording of every field message. The messages reach the
	client and are covered by the handler test where it matters; pinning each
	string here would break on a reword while proving nothing new.

	The one exception is the inversion, where the message IS the assertion: the
	point of naming the provided side is that the caller is told about a field
	they actually sent, so a test that checked only the field name would pass
	with the two messages swapped.

Additional Remarks:

	The pointer fields are the whole reason this file exists separately from
	create's value-typed input: absent (nil) must be distinct from provided so a
	merge leaves omitted fields alone. new(v) (Go 1.26) builds those pointers inline.
*/
package dhcpwindows

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/apierror"
)

// existingScope is the scope an update is applied against: a /24 with a pool
// well inside it, so a resize has room to stay in-subnet or step out of it.
func existingScope() Scope {
	return Scope{
		ScopeID:              "10.0.30.0",
		SubnetMask:           "255.255.255.0",
		StartRange:           "10.0.30.10",
		EndRange:             "10.0.30.250",
		Name:                 "lab",
		State:                "Active",
		Type:                 "Dhcp",
		LeaseDurationSeconds: 691200,
	}
}

// updateFieldNamesOf lists the fields a validation result complains about.
func updateFieldNamesOf(t *testing.T, in ScopeUpdate) []string {
	t.Helper()

	errs := in.Validate()

	names := make([]string, 0, len(errs))
	for _, e := range errs {
		names = append(names, e.Field)
	}

	return names
}

func TestScopeUpdate_ShouldAcceptAWellFormedUpdate(t *testing.T) {
	t.Parallel()

	// ARRANGE — every field set, including a non-ASCII name, so the optional
	// paths are covered rather than skipped.
	in := ScopeUpdate{
		Name:                 new("Standort München"),
		Description:          new("reconciled by weave"),
		LeaseDurationSeconds: new(3600),
		State:                new("Inactive"),
		Type:                 new("Both"),
		StartRange:           new("10.0.30.20"),
		EndRange:             new("10.0.30.200"),
	}

	// ACT / ASSERT
	assert.Empty(t, in.Validate())
}

func TestScopeUpdate_ShouldAcceptAnEmptyUpdate(t *testing.T) {
	t.Parallel()

	// ARRANGE — nothing set. A merge with no fields is a no-op, not an error;
	// Validate's job is to reject malformed values, and there are none.
	// ACT / ASSERT
	assert.Empty(t, ScopeUpdate{}.Validate())
}

func TestScopeUpdate_ShouldRejectEachMalformedField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        ScopeUpdate
		wantField string
	}{
		{"long name", ScopeUpdate{Name: new(strings.Repeat("n", maxNameLength+1))}, "name"},
		{"control char in name", ScopeUpdate{Name: new("a\x00b")}, "name"},
		{"newline in description", ScopeUpdate{Description: new("line one\nline two")}, "description"},
		{"long description", ScopeUpdate{Description: new(strings.Repeat("d", maxDescriptionLength+1))}, "description"},
		{"zero lease", ScopeUpdate{LeaseDurationSeconds: new(0)}, "leaseDurationSeconds"},
		{"negative lease", ScopeUpdate{LeaseDurationSeconds: new(-1)}, "leaseDurationSeconds"},
		{"lease above int32", ScopeUpdate{LeaseDurationSeconds: new(int(math.MaxInt32) + 1)}, "leaseDurationSeconds"},
		{"unknown state", ScopeUpdate{State: new("Paused")}, "state"},
		{"unknown type", ScopeUpdate{Type: new("Gopher")}, "type"},
		{"non-address start", ScopeUpdate{StartRange: new("nope")}, "startRange"},
		{"IPv6 start", ScopeUpdate{StartRange: new("2001:db8::1")}, "startRange"},
		{"non-address end", ScopeUpdate{EndRange: new("nope")}, "endRange"},
		{
			"end before start",
			ScopeUpdate{StartRange: new("10.0.30.200"), EndRange: new("10.0.30.10")},
			"endRange",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT / ASSERT — the named field is reported.
			assert.Contains(t, updateFieldNamesOf(t, tt.in), tt.wantField)
		})
	}
}

func TestScopeUpdate_ShouldRejectAPresentButEmptyFreeTextField(t *testing.T) {
	t.Parallel()

	// ARRANGE — an empty string cannot be told from absent once it reaches the
	// script's if ($env:...) guard, so clearing a free-text field is a 400 in
	// this version rather than a silent no-op that reads as a clear.
	// ACT
	names := updateFieldNamesOf(t, ScopeUpdate{Name: new(""), Description: new("")})

	// ASSERT
	assert.ElementsMatch(t, []string{"name", "description"}, names)
}

func TestScopeUpdate_ShouldReportEveryFailureAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE — several fields wrong in one update.
	in := ScopeUpdate{
		Name:                 new(""),
		LeaseDurationSeconds: new(-1),
		State:                new("Paused"),
		StartRange:           new("nope"),
	}

	// ACT / ASSERT — all of them, so a client fixes every mistake in one round trip.
	assert.ElementsMatch(t,
		[]string{"name", "leaseDurationSeconds", "state", "startRange"},
		updateFieldNamesOf(t, in))
}

func TestScopeUpdateEnv_ShouldSplatOnlyProvidedFields(t *testing.T) {
	t.Parallel()

	// ARRANGE — only the name is set.
	in := ScopeUpdate{Name: new("renamed")}

	// ACT
	env := in.env(existingScope())

	// ASSERT — the scope id is always the -ScopeId target; the provided name is
	// present; every unset field is absent so the script leaves it unchanged.
	assert.Equal(t, "10.0.30.0", env[envScopeID])
	assert.Equal(t, "renamed", env[envScopeName])

	for _, absent := range []string{
		envScopeDescription, envScopeState, envScopeType, envScopeLease,
		envScopeStartRange, envScopeEndRange,
	} {
		assert.NotContains(t, env, absent)
	}
}

func TestScopeUpdateEnv_ShouldSplatBothRangeEndsWhenEitherIsProvided(t *testing.T) {
	t.Parallel()

	// ARRANGE — only the start is being changed.
	in := ScopeUpdate{StartRange: new("10.0.30.20")}

	// ACT
	env := in.env(existingScope())

	// ASSERT — Set-DhcpServerv4Scope's -StartRange and -EndRange are a
	// mandatory-together parameter set, so a one-sided change must still carry
	// both. The end the caller omitted is filled from the existing scope.
	assert.Equal(t, "10.0.30.20", env[envScopeStartRange])
	assert.Equal(t, "10.0.30.250", env[envScopeEndRange],
		"the omitted end is filled from the existing scope, so the pair is always complete")
}

// fieldNames lists the fields a set of validation failures complains about, so
// a test can assert on which fields were named without restating every message.
func fieldNames(errs []apierror.FieldError) []string {
	names := make([]string, 0, len(errs))
	for _, e := range errs {
		names = append(names, e.Field)
	}

	return names
}

func TestScopeUpdate_ShouldAcceptAResizeWithinTheSubnet(t *testing.T) {
	t.Parallel()

	// ARRANGE — a resize that stays inside 10.0.30.0/24.
	in := ScopeUpdate{StartRange: new("10.0.30.5"), EndRange: new("10.0.30.200")}

	// ACT
	offending, err := in.validateEffectiveRange(existingScope())

	// ASSERT — nothing leaves the subnet, so the identity does not move.
	require.NoError(t, err)
	assert.Empty(t, offending)
}

func TestScopeUpdate_ShouldNameARangeEndOnTheNetworkOrBroadcastAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ScopeUpdate
		want []string
	}{
		{
			name: "start on the network address",
			in:   ScopeUpdate{StartRange: new("10.0.30.0")},
			want: []string{"startRange"},
		},
		{
			name: "end on the broadcast address",
			in:   ScopeUpdate{EndRange: new("10.0.30.255")},
			want: []string{"endRange"},
		},
		{
			name: "both at once",
			in:   ScopeUpdate{StartRange: new("10.0.30.0"), EndRange: new("10.0.30.255")},
			want: []string{"startRange", "endRange"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			offending, err := tt.in.validateEffectiveRange(existingScope())

			// ASSERT — named, so the resize is a 400 rather than the 502 the
			// thrown Set-DhcpServerv4Scope exception produced.
			require.NoError(t, err)
			assert.Equal(t, tt.want, fieldNames(offending))
		})
	}
}

func TestScopeUpdate_ShouldNotJudgeLeasabilityOfAnUnparseableEnd(t *testing.T) {
	t.Parallel()

	// ARRANGE — an end Validate would already have refused, reaching the subnet
	// check anyway. The zero netip.Addr has no bytes to mask, so this is the
	// path that must skip the leasable check rather than panic in it.
	in := ScopeUpdate{EndRange: new("not-an-address")}

	// ACT
	offending, err := in.validateEffectiveRange(existingScope())

	// ASSERT — named exactly once.
	require.NoError(t, err)
	assert.Equal(t, []string{"endRange"}, fieldNames(offending))
}

func TestScopeUpdate_ShouldNameARangeFieldThatLeavesTheSubnet(t *testing.T) {
	t.Parallel()

	// ARRANGE — a new end in the next subnet, with the start left unchanged. The
	// check uses the existing start for the side the caller omitted, so only the
	// end is reported.
	in := ScopeUpdate{EndRange: new("10.0.31.10")}

	// ACT
	offending, err := in.validateEffectiveRange(existingScope())

	// ASSERT — the offending field is named so the handler can render a precise
	// 400; the in-subnet start is not.
	require.NoError(t, err)
	assert.Equal(t, []string{"endRange"}, fieldNames(offending))
}

func TestScopeUpdate_ShouldRejectAOneSidedResizeThatInvertsTheRange(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in          ScopeUpdate
		wantField   string
		wantMessage string
	}{
		// The existing scope runs 10.0.30.10-10.0.30.250. Both of these are in
		// the subnet, both are leasable addresses, and both describe a range
		// that runs backwards once the untouched end is filled in.
		"an end below the untouched start": {
			in:          ScopeUpdate{EndRange: new("10.0.30.5")},
			wantField:   "endRange",
			wantMessage: "must not be before startRange",
		},
		"a start above the untouched end": {
			in:          ScopeUpdate{StartRange: new("10.0.30.251")},
			wantField:   "startRange",
			wantMessage: "must not be after endRange",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT
			offending, err := tt.in.validateEffectiveRange(existingScope())

			// ASSERT
			// A 400 naming the field the caller actually sent. Without this the
			// update reached Set-DhcpServerv4Scope with start > end, the cmdlet
			// threw, $ErrorActionPreference = 'Stop' exited non-zero, and
			// runError classified it ErrBackendUnavailable — so a client typo
			// came back as a 502 and a BACKEND-101 at ERROR naming a DHCP
			// server that was working perfectly.
			require.NoError(t, err)
			require.Len(t, offending, 1)
			assert.Equal(t, tt.wantField, offending[0].Field)

			// The message is pinned here, unlike everywhere else in this file:
			// naming the provided side is the whole behaviour, and a test that
			// checked the field alone would pass with the two swapped.
			assert.Equal(t, tt.wantMessage, offending[0].Message)
		})
	}
}

func TestScopeUpdate_ShouldNameAFieldOnceWhenItBreaksTwoRules(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// An end in the previous subnet: outside 10.0.30.0/24 AND below the
	// untouched start. One mistake, and Windows would refuse it for either
	// reason.
	in := ScopeUpdate{EndRange: new("10.0.29.5")}

	// ACT
	offending, err := in.validateEffectiveRange(existingScope())

	// ASSERT
	// Named once, for the first rule it broke. Reporting the same field twice
	// reads as two problems and is one — the same reason the subnet check
	// already deduplicated against the leasable one.
	require.NoError(t, err)
	require.Len(t, offending, 1)
	assert.Equal(t, "endRange", offending[0].Field)
	assert.Contains(t, offending[0].Message, "existing subnet")
}
