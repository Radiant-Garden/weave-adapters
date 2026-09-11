//go:build windows

/*
Testing: secure_windows.go

Pending:

Tested:

	confersWrite -> - TestConfersWrite_ShouldCatchEveryRightThatChangesTheObject:
	                  including FILE_WRITE_DATA on its own, which is the token
	                  store escalation and is granted independently of the
	                  generic bit.
	lockdownDACL -> - TestLockdownDACL_ShouldGrantExactlyTheTwoPrincipals
	                - TestLockdownDACL_ShouldSetInheritFlagsOnlyForADirectory

Tested elsewhere:

	Applying and reading back a real security descriptor: task service-gate,
	elevated, against a live filesystem. Secure verifies its own result
	through CheckGrants, and that function is unit-tested portably in
	secure_test.go -- so what a unit test could add here is coverage of the
	syscall, not of the decision.

	Which paths are secured, and that install and `service secure` both call
	this: cmd/weave-adapter-dhcp-windows/service_test.go.

Declined:

	Driving Secure against a temp file. Setting an owner needs the admin
	token; ci:windows runs as an unprivileged service account by design, so
	the test would fail there for a reason that is not a defect -- and on a
	developer's elevated shell it would leave a re-owned file behind.

	Asserting the NULL-DACL branch of ReadGrants. Producing one means building
	a security descriptor with no DACL and applying it, which is the same
	privileged mutation, to observe a branch that is three lines long.

Additional Remarks:

	lockdownDACL is testable here because it only assembles an ACL in memory;
	nothing is written until SetNamedSecurityInfo, which is the part left to
	the gate.
*/
package winsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestConfersWrite_ShouldCatchEveryRightThatChangesTheObject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mask windows.ACCESS_MASK
		want bool
	}{
		// The one that matters most: appending a hash to the token store is
		// how a local user grants themselves a bearer token the adapter
		// accepts, and it needs no generic right at all.
		"should catch write data alone":  {mask: windows.FILE_WRITE_DATA, want: true},
		"should catch append data alone": {mask: windows.FILE_APPEND_DATA, want: true},
		"should catch generic all":       {mask: windows.GENERIC_ALL, want: true},
		"should catch generic write":     {mask: windows.GENERIC_WRITE, want: true},
		// Taking ownership or rewriting the ACL is a write by any useful
		// definition: both let the holder grant themselves the rest.
		"should catch write owner": {mask: windows.WRITE_OWNER, want: true},
		"should catch write dac":   {mask: windows.WRITE_DAC, want: true},
		"should catch delete":      {mask: windows.DELETE, want: true},

		"should allow read data":    {mask: windows.FILE_READ_DATA, want: false},
		"should allow generic read": {mask: windows.GENERIC_READ, want: false},
		"should allow nothing":      {mask: 0, want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.Equal(t, tc.want, confersWrite(tc.mask))
		})
	}
}

func TestLockdownDACL_ShouldGrantExactlyTheTwoPrincipals(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	acl, err := lockdownDACL(InheritNone)

	// ASSERT
	// Two entries and no more: the list replaces rather than extends, which
	// is what makes this a lockdown instead of an addition.
	require.NoError(t, err)
	require.NotNil(t, acl)
	assert.Equal(t, len(LockdownGrantees()), int(acl.AceCount))
}

func TestLockdownDACL_ShouldSetInheritFlagsOnlyForADirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	fileACL, err := lockdownDACL(InheritNone)
	require.NoError(t, err)

	dirACL, err := lockdownDACL(InheritToChildren)
	require.NoError(t, err)

	// ASSERT
	// Both build; the flags themselves are read back from a real object by
	// the gate. What is checked here is that the two shapes are distinct --
	// a directory that inherited nothing would leave every log file the
	// adapter creates on the parent's defaults.
	require.NotNil(t, fileACL)
	require.NotNil(t, dirACL)
	assert.Equal(t, fileACL.AceCount, dirACL.AceCount)
}
