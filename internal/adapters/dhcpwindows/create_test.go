/*
Testing: create.go

Pending:

	A create against a real WS2022 host. Everything here runs against the fake
	runner, so it proves what the adapter sends and how it reads the answer; it
	cannot prove Add-DhcpServerv4Scope accepts that parameter set. The e2e gate
	covers it once the endpoint exists.

	Whether -PassThru returns the projection this expects. The fixture is
	hand-written, unlike the read path's, which was captured from the host.
	Capture a real one at sign-off.

Tested:

	ScopeInput.Validate
	  - TestValidate_ShouldAcceptAWellFormedInput: the happy path reports nothing, including a non-ASCII name.
	  - TestValidate_ShouldAcceptTheContiguousMaskEdges: /30, /31 and /32 are contiguous and accepted — the boundary isContiguousMask is likeliest to misjudge.
	  - TestValidate_ShouldRejectEachMalformedField: every rule, one case each — including the int32 lease bound and control-character rejection that keep a client mistake from reading as a 502.
	  - TestValidate_ShouldReportEveryFailureAtOnce: a wholly invalid input names all its fields.
	ScopeInput.ScopeID
	  - TestScopeID_ShouldBeTheNetworkAddress: start masked by subnetMask, for several masks.
	ScopeInput.env
	  - TestEnv_ShouldOmitUnsetOptionalsRatherThanSendingEmpty: omitted means Windows' default.
	Client.CreateScope
	  - TestCreateScope_ShouldReturnTheCreatedScopeWithItsIdentity: decode, derive, serve.
	  - TestCreateScope_ShouldNeverInterpolateInputIntoTheScript: the injection property.
	  - TestCreateScope_ShouldReportAConflictWhenTheSubnetIsTaken: the typed error a 409 renders from.
	  - TestCreateScope_ShouldRejectAScopeOnADifferentSubnetThanAsked: the Location-would-lie guard.
	  - TestCreateScope_ShouldRejectAPayloadThatIsNotExactlyOneScope: -PassThru returns one or the script misbehaved.
	  - TestCreateScope_ShouldNotMistakeAScopeDescriptionForTheConflictMarker: the marker is matched exactly, so a payload containing it is still a create.

Tested elsewhere:

	Decoding, the empty-stdout and bare-null rules, and the wadaptID derivation
	itself: client_test.go and identity_test.go. This file asserts only what
	create adds.

	That the runner passes env to the child at all: runner_test.go, against a
	real process.

Declined:

	Asserting the PowerShell text beyond "it does not contain the input". The
	script is a constant; pinning its wording would fail on every edit while
	proving nothing a fake runner can observe.

	Exercising the create/create race the pre-check cannot close. It needs two
	real concurrent Add-DhcpServerv4Scope calls against one subnet; the fake
	cannot produce it, and the behaviour under it is documented rather than
	claimed.

Additional Remarks:

	TestCreateScope_ShouldNeverInterpolateInputIntoTheScript is the one to keep
	if the file ever has to shrink. Everything else here is correctness; that one
	is the security property the whole env-passing design exists to provide, and
	it fails loudly the moment someone "simplifies" the script by splicing a
	value into it.
*/
package dhcpwindows

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validInput is a create that should pass every rule, so a test can vary one
// field and attribute the failure to it.
func validInput() ScopeInput {
	return ScopeInput{
		Name:       "lab-vlan-30",
		StartRange: "10.0.30.10",
		EndRange:   "10.0.30.250",
		SubnetMask: "255.255.255.0",
	}
}

// createdScopeJSON is what the script emits after a successful create: the same
// projection the read path uses, wrapped in an array by @().
const createdScopeJSON = `[
    {
        "scopeId":  "10.0.30.0",
        "subnetMask":  "255.255.255.0",
        "startRange":  "10.0.30.10",
        "endRange":  "10.0.30.250",
        "name":  "lab-vlan-30",
        "description":  "",
        "state":  "Active",
        "type":  "Dhcp",
        "superscopeName":  "",
        "leaseDurationSeconds":  691200
    }
]`

// fieldNamesOf lists the fields a validation result complains about.
func fieldNamesOf(t *testing.T, in ScopeInput) []string {
	t.Helper()

	errs := in.Validate()

	names := make([]string, 0, len(errs))
	for _, e := range errs {
		names = append(names, e.Field)
	}

	return names
}

func TestValidate_ShouldAcceptAWellFormedInput(t *testing.T) {
	t.Parallel()

	// ARRANGE — every optional field set too, so the optional paths are covered
	// rather than merely skipped. The name carries a non-ASCII character on
	// purpose: the control-character rule must reject C0/C1 without touching
	// legitimate Unicode, and "München" is the value the whole encoding chain
	// exists to carry intact.
	in := validInput()
	in.Name = "Standort München"
	in.Description = "created by weave"
	in.LeaseDurationSeconds = 3600
	in.State = "Inactive"
	in.Type = "Both"

	// ACT / ASSERT
	assert.Empty(t, in.Validate())
}

func TestValidate_ShouldAcceptTheContiguousMaskEdges(t *testing.T) {
	t.Parallel()

	// ARRANGE — the tightest masks are the ones isContiguousMask is most likely
	// to get wrong: /31 and /32 are all-ones runs with a zero or one-bit tail,
	// exactly the boundary the power-of-two trick turns on. They pass through
	// validateAddressing untested elsewhere, and Windows rejects a bad mask late
	// with a message about a cmdlet parameter, so accepting the good ones here is
	// what keeps that rejection from ever being reached for a legitimate input.
	tests := []struct {
		name  string
		mask  string
		start string
		end   string
	}{
		{name: "/30", mask: "255.255.255.252", start: "192.168.1.4", end: "192.168.1.6"},
		{name: "/31", mask: "255.255.255.254", start: "192.168.1.4", end: "192.168.1.5"},
		{name: "/32", mask: "255.255.255.255", start: "192.168.1.4", end: "192.168.1.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			in := validInput()
			in.SubnetMask = tt.mask
			in.StartRange = tt.start
			in.EndRange = tt.end

			// ACT / ASSERT — a contiguous mask, however tight, is accepted.
			assert.Empty(t, in.Validate(), "%s should be a valid contiguous mask", tt.mask)
		})
	}
}

func TestValidate_ShouldRejectEachMalformedField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*ScopeInput)
		wantField string
	}{
		{
			name:      "should require a name",
			mutate:    func(in *ScopeInput) { in.Name = "" },
			wantField: "name",
		},
		{
			name:      "should bound the name",
			mutate:    func(in *ScopeInput) { in.Name = strings.Repeat("n", maxNameLength+1) },
			wantField: "name",
		},
		{
			name:      "should bound the description",
			mutate:    func(in *ScopeInput) { in.Description = strings.Repeat("d", maxDescriptionLength+1) },
			wantField: "description",
		},
		{
			name:      "should reject a non-address start",
			mutate:    func(in *ScopeInput) { in.StartRange = "not-an-address" },
			wantField: "startRange",
		},
		{
			// M3a serves IPv4 only, so a v6 address is as unusable as a word.
			name:      "should reject an IPv6 start",
			mutate:    func(in *ScopeInput) { in.StartRange = "2001:db8::1" },
			wantField: "startRange",
		},
		{
			name:      "should reject a non-address mask",
			mutate:    func(in *ScopeInput) { in.SubnetMask = "nope" },
			wantField: "subnetMask",
		},
		{
			// Parses as an address, is not a mask. Windows would reject it with
			// a message about a cmdlet parameter the client never sent.
			name:      "should reject a non-contiguous mask",
			mutate:    func(in *ScopeInput) { in.SubnetMask = "255.255.0.255" },
			wantField: "subnetMask",
		},
		{
			name:      "should reject a zero mask",
			mutate:    func(in *ScopeInput) { in.SubnetMask = "0.0.0.0" },
			wantField: "subnetMask",
		},
		{
			name:      "should reject an end before the start",
			mutate:    func(in *ScopeInput) { in.EndRange = "10.0.30.5" },
			wantField: "endRange",
		},
		{
			// The subnet is what the scope's identity derives from, so a range
			// spanning two of them would create a scope that is not the one
			// described.
			name:      "should reject an end in another subnet",
			mutate:    func(in *ScopeInput) { in.EndRange = "10.0.31.10" },
			wantField: "endRange",
		},
		{
			name:      "should reject a negative lease",
			mutate:    func(in *ScopeInput) { in.LeaseDurationSeconds = -1 },
			wantField: "leaseDurationSeconds",
		},
		{
			// Above int32, the script's [int] cast throws, the shell exits
			// non-zero, and the client is told the backend is unreachable — a
			// false outage from a client mistake. Bounded here so it is a 400.
			//
			// The add happens at runtime, through a variable, so it is not a
			// constant expression the compiler would reject as an int overflow on
			// a 32-bit build (this package must still compile there even though it
			// only ships for amd64). On a 64-bit int the value is genuinely over
			// the bound; the field is named either way.
			name: "should reject a lease above int32",
			mutate: func(in *ScopeInput) {
				max32 := int(math.MaxInt32)
				in.LeaseDurationSeconds = max32 + 1
			},
			wantField: "leaseDurationSeconds",
		},
		{
			// A NUL never reaches PowerShell — os/exec rejects the env value — so
			// without this guard it is a 502 rather than the 400 it is.
			name:      "should reject a NUL in the name",
			mutate:    func(in *ScopeInput) { in.Name = "a\x00b" },
			wantField: "name",
		},
		{
			// A newline does reach Windows and lands verbatim as a scope name in
			// the console, so it is rejected before it can.
			name:      "should reject a control character in the description",
			mutate:    func(in *ScopeInput) { in.Description = "line one\nline two" },
			wantField: "description",
		},
		{
			name:      "should reject an unknown state",
			mutate:    func(in *ScopeInput) { in.State = "Paused" },
			wantField: "state",
		},
		{
			name:      "should reject an unknown type",
			mutate:    func(in *ScopeInput) { in.Type = "Gopher" },
			wantField: "type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			in := validInput()
			tt.mutate(&in)

			// ACT / ASSERT — the named field is reported, so a client is told
			// which value to fix rather than that something was wrong.
			assert.Contains(t, fieldNamesOf(t, in), tt.wantField)
		})
	}
}

func TestValidate_ShouldReportEveryFailureAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE — nothing usable at all.
	in := ScopeInput{
		Name:       "",
		StartRange: "x",
		EndRange:   "y",
		SubnetMask: "z",
		State:      "Paused",
		Type:       "Gopher",
	}

	// ACT
	fields := fieldNamesOf(t, in)

	// ASSERT — all of them. Returning the first would send a client back for one
	// round trip per mistake, which is what the errors[] extension exists to
	// prevent.
	assert.ElementsMatch(t,
		[]string{"name", "startRange", "endRange", "subnetMask", "state", "type"},
		fields)
}

func TestScopeID_ShouldBeTheNetworkAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, start, mask, want string
	}{
		{name: "class C", start: "10.0.30.10", mask: "255.255.255.0", want: "10.0.30.0"},
		{name: "class B", start: "172.16.5.99", mask: "255.255.0.0", want: "172.16.0.0"},
		{name: "class A", start: "10.9.8.7", mask: "255.0.0.0", want: "10.0.0.0"},
		{name: "/23 spans an octet", start: "10.0.5.10", mask: "255.255.254.0", want: "10.0.4.0"},
		{name: "/30", start: "192.168.1.6", mask: "255.255.255.252", want: "192.168.1.4"},
		{name: "already the network address", start: "192.168.178.0", mask: "255.255.255.0", want: "192.168.178.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			in := ScopeInput{StartRange: tt.start, SubnetMask: tt.mask}

			// ACT
			got, err := in.ScopeID()

			// ASSERT — this value is what wadaptID derives from and what the
			// Location header will name, so it has to match what Windows itself
			// computes from the same two inputs.
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnv_ShouldOmitUnsetOptionalsRatherThanSendingEmpty(t *testing.T) {
	t.Parallel()

	// ARRANGE — only the required fields.
	in := validInput()

	// ACT
	env := in.env("10.0.30.0")

	// ASSERT — the script decides whether to splat each optional by testing its
	// variable for emptiness, so an unset field must be absent. Passing an empty
	// Description would set a description of "" instead of leaving Windows to
	// apply its own default, and the difference is invisible until someone reads
	// the scope back.
	for _, absent := range []string{envScopeDescription, envScopeState, envScopeType, envScopeLease} {
		assert.NotContains(t, env, absent)
	}

	assert.Equal(t, "10.0.30.0", env[envScopeID])
	assert.Equal(t, "lab-vlan-30", env[envScopeName])

	// And present once set.
	in.Description = "created by weave"
	in.LeaseDurationSeconds = 3600
	assert.Equal(t, "created by weave", in.env("10.0.30.0")[envScopeDescription])
	assert.Equal(t, "3600", in.env("10.0.30.0")[envScopeLease])
}

func TestCreateScope_ShouldReturnTheCreatedScopeWithItsIdentity(t *testing.T) {
	t.Parallel()

	// ARRANGE
	client := clientWith(&fakeRunner{stdout: []byte(createdScopeJSON)})

	// ACT
	scope, err := client.CreateScope(context.Background(), validInput())

	// ASSERT — the created scope comes back carrying the identity a Location
	// header will name, with no read-back: the ID is a function of inputs the
	// caller already supplied.
	require.NoError(t, err)
	assert.Equal(t, "10.0.30.0", scope.ScopeID)
	assert.Len(t, scope.WadaptID, WadaptIDLength)
	assert.Equal(t, AddressFamilyIPv4, scope.AddressFamily)
	assert.Equal(t, "lab-vlan-30", scope.Name)
}

func TestCreateScope_ShouldNeverInterpolateInputIntoTheScript(t *testing.T) {
	t.Parallel()

	// ARRANGE — a description carrying a PowerShell payload. Spliced into the
	// script text this would execute, because powershell.exe -Command takes one
	// string and a value inside it is code.
	const payload = `'; Remove-DhcpServerv4Scope -ScopeId 10.0.0.0 ;#`

	in := validInput()
	in.Name = `"; Stop-Service DHCPServer ;#`
	in.Description = payload

	fake := &fakeRunner{stdout: []byte(createdScopeJSON)}

	// ACT
	_, err := clientWith(fake).CreateScope(context.Background(), in)
	require.NoError(t, err)

	// ASSERT — the script the runner was handed is the constant, and every
	// caller value reached the child through the environment instead.
	require.Len(t, fake.scripts, 1)
	assert.Equal(t, createScopeScript, fake.scripts[0],
		"the script must be the constant; nothing may be interpolated into it")

	assert.NotContains(t, fake.scripts[0], "Remove-DhcpServerv4Scope")
	assert.NotContains(t, fake.scripts[0], "Stop-Service")

	require.Len(t, fake.envs, 1)
	assert.Equal(t, payload, fake.envs[0][envScopeDescription],
		"the payload travels as data, unaltered")
}

func TestCreateScope_ShouldReportAConflictWhenTheSubnetIsTaken(t *testing.T) {
	t.Parallel()

	// ARRANGE — the marker the script prints when it finds the subnet occupied.
	client := clientWith(&fakeRunner{stdout: []byte(conflictMarker + "\r\n")})

	// ACT
	_, err := client.CreateScope(context.Background(), validInput())

	// ASSERT — a typed error, so the handler can answer 409 rather than a
	// generic backend failure. The subnet is named, because "which one" is the
	// first thing anyone asks.
	require.ErrorIs(t, err, ErrScopeExists)
	assert.Contains(t, err.Error(), "10.0.30.0")
}

func TestCreateScope_ShouldNotMistakeAScopeDescriptionForTheConflictMarker(t *testing.T) {
	t.Parallel()

	// ARRANGE — a successful create whose payload happens to contain the marker
	// text. A substring search would read this as a conflict and report a
	// failure for a scope that was created.
	payload := strings.Replace(createdScopeJSON, `"description":  ""`,
		`"description":  "`+conflictMarker+`"`, 1)

	// ACT
	scope, err := clientWith(&fakeRunner{stdout: []byte(payload)}).
		CreateScope(context.Background(), validInput())

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, conflictMarker, scope.Description)
}

func TestCreateScope_ShouldRejectAScopeOnADifferentSubnetThanAsked(t *testing.T) {
	t.Parallel()

	// ARRANGE — the backend reports having created a different subnet than the
	// one requested.
	elsewhere := strings.Replace(createdScopeJSON, `"scopeId":  "10.0.30.0"`,
		`"scopeId":  "10.0.99.0"`, 1)

	// ACT
	_, err := clientWith(&fakeRunner{stdout: []byte(elsewhere)}).
		CreateScope(context.Background(), validInput())

	// ASSERT — refused rather than served. The identity in a Location header is
	// derived from the subnet, so serving this would hand a client a URL naming
	// a resource it did not create.
	require.ErrorIs(t, err, ErrBackendMalformed)
	assert.Contains(t, err.Error(), "10.0.99.0")
}

func TestCreateScope_ShouldRejectAPayloadThatIsNotExactlyOneScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "should reject an empty result", stdout: `[]`},
		{
			name:   "should reject two scopes",
			stdout: `[{"scopeId":"10.0.30.0"},{"scopeId":"10.0.31.0"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// ACT
			_, err := clientWith(&fakeRunner{stdout: []byte(tt.stdout)}).
				CreateScope(context.Background(), validInput())

			// ASSERT — -PassThru returns exactly what it created, so anything
			// else means the script did something other than what it was written
			// to do. Serving a scope out of that would report a create that may
			// not have happened as described.
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBackendMalformed)
		})
	}
}
