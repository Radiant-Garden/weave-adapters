/*
Testing: secure.go

Pending:

Tested:

	LockdownGrantees -> - TestLockdownGrantees_ShouldBeSystemAndAdministratorsOnly:
	                      including that LocalSystem is S-1-5-18, not the
	                      S-1-5-20 the retired script defaulted to.
	SecurablesFor    -> - TestSecurablesFor_ShouldAlwaysCoverTheConfigFile
	                    - TestSecurablesFor_ShouldSecureTheLogDirectoryNotTheLogFile
	                    - TestSecurablesFor_ShouldSkipWhatIsNotConfigured
	                    - TestSecurablesFor_ShouldMarkOnlyTheTokenStoreOptional
	Securable.Inheritance -> - TestSecurableInheritance_ShouldPropagateOnlyForDirectories
	CheckGrants      -> - TestCheckGrants_ShouldAcceptThePolicyGrantees
	                    - TestCheckGrants_ShouldRejectAnOutsideWrite
	                    - TestCheckGrants_ShouldIgnoreAnOutsideRead
	                    - TestCheckGrants_ShouldMatchSIDsCaseInsensitively
	                    - TestCheckGrants_ShouldNameEveryOffenderAndTheFix

Tested elsewhere:

	Applying the policy, and reading a real access list back: secure_windows.go
	against a live filesystem, exercised by task service-gate. The apply
	verifies its own result through CheckGrants, so the function tested here is
	the one that decides whether a lockdown took.

	That install and `service secure` call it with the resolved paths:
	cmd/weave-adapter-dhcp-windows/service_test.go.

Declined:

	Testing SetNamedSecurityInfo. It mutates a real security descriptor and
	needs Administrator; a unit test would either fail on the unprivileged
	ci:windows runner or leave a re-owned file behind.

Additional Remarks:

	The policy is portable data on purpose. It replaces
	scripts/secure-token-store.ps1, whose defaults had drifted from the
	milestone's decisions -- it granted NETWORK SERVICE, defaulted to a
	relative path Phase 3 now rejects, and secured files only, so a log file
	created later inherited nothing from it.

	Only writes are refused, not reads. The token store holds hashes, which
	cannot be replayed, and a rule that failed on a benign backup or auditing
	read entry is a rule that gets switched off.
*/
package winsvc

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockdownGrantees_ShouldBeSystemAndAdministratorsOnly(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	got := LockdownGrantees()

	// ASSERT
	// S-1-5-18 is LocalSystem, which is what the service runs as. The retired
	// script defaulted to S-1-5-20 (NETWORK SERVICE), an assumption that
	// predates the LocalSystem decision and was disproved by measurement:
	// NETWORK SERVICE was refused WIN32 5 against the DHCP cmdlets in every
	// group tried on WS2022.
	assert.Equal(t, []string{"S-1-5-18", "S-1-5-32-544"}, got)

	// SIDs, never names: a name is locale-dependent, and on a German host the
	// administrators group is "Administratoren".
	for _, sid := range got {
		assert.Regexp(t, `^S-1-`, sid, "the policy must be written in SIDs")
	}
}

func TestSecurablesFor_ShouldAlwaysCoverTheConfigFile(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	got := SecurablesFor(`C:\cfg\config.toml`, "", "")

	// ASSERT
	// It carries identity.namespaceKey: a read leaks what every wadaptID on
	// the host derives from, and a write re-keys the fleet.
	require.Len(t, got, 1)
	assert.Equal(t, `C:\cfg\config.toml`, got[0].Path)
	assert.Equal(t, SecurableFile, got[0].Kind)
	assert.False(t, got[0].Optional, "the config file must exist by install time")
	assert.Contains(t, got[0].Why, "namespaceKey")
}

func TestSecurablesFor_ShouldSecureTheLogDirectoryNotTheLogFile(t *testing.T) {
	t.Parallel()

	// ARRANGE
	logFile := filepath.Join("C:", "logs", "adapter.log")

	// ACT
	got := SecurablesFor(`C:\cfg\config.toml`, "", logFile)

	// ASSERT
	// The adapter creates the log itself, at runtime, and a new file takes its
	// parent's inheritable entries. Securing the file would leave the next one
	// created under whatever the directory permits.
	require.Len(t, got, 2)
	assert.Equal(t, filepath.Dir(logFile), got[1].Path)
	assert.Equal(t, SecurableDirectory, got[1].Kind)
	assert.Equal(t, InheritToChildren, got[1].Inheritance())
}

func TestSecurablesFor_ShouldSkipWhatIsNotConfigured(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	// An empty logFile means stdout, and an empty token store means auth is
	// off. Securing the current directory because a key was unset would lock
	// down whatever the operator happened to be standing in.
	got := SecurablesFor(`C:\cfg\config.toml`, "", "")

	// ASSERT
	assert.Len(t, got, 1)
}

func TestSecurablesFor_ShouldMarkOnlyTheTokenStoreOptional(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	got := SecurablesFor(`C:\cfg\config.toml`, `C:\cfg\tokens.toml`, `C:\logs\a.log`)

	// ASSERT
	// The store may not exist yet: an operator can install before minting a
	// token. The config file and the log directory must both be there.
	require.Len(t, got, 3)
	assert.False(t, got[0].Optional)
	assert.True(t, got[1].Optional, "the token store may not have been minted yet")
	assert.False(t, got[2].Optional)
}

func TestSecurableInheritance_ShouldPropagateOnlyForDirectories(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Equal(t, InheritNone, Securable{Kind: SecurableFile}.Inheritance())
	assert.Equal(t, InheritToChildren, Securable{Kind: SecurableDirectory}.Inheritance())
}

func TestCheckGrants_ShouldAcceptThePolicyGrantees(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.NoError(t, CheckGrants(`C:\cfg\tokens.toml`, []Grant{
		{SID: SIDLocalSystem, CanWrite: true},
		{SID: SIDAdministrators, CanWrite: true},
	}))
}

func TestCheckGrants_ShouldRejectAnOutsideWrite(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// S-1-5-32-545 is Users. A write here is a local privilege escalation into
	// the API: the adapter trusts every hash in the store it reads at startup.
	grants := []Grant{
		{SID: SIDLocalSystem, CanWrite: true},
		{SID: "S-1-5-32-545", CanWrite: true},
	}

	// ACT
	err := CheckGrants(`C:\cfg\tokens.toml`, grants)

	// ASSERT
	require.ErrorIs(t, err, ErrNotSecured)
	assert.Contains(t, err.Error(), "S-1-5-32-545")
}

func TestCheckGrants_ShouldIgnoreAnOutsideRead(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckGrants(`C:\cfg\tokens.toml`, []Grant{
		{SID: SIDLocalSystem, CanWrite: true},
		{SID: "S-1-5-32-545", CanWrite: false},
	})

	// ASSERT
	// A read of the store leaks hashes, which cannot be replayed as tokens,
	// and failing on a benign backup or auditing entry would make this a rule
	// operators switch off rather than satisfy.
	assert.NoError(t, err)
}

func TestCheckGrants_ShouldMatchSIDsCaseInsensitively(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	// Windows renders a SID string in upper case but accepts either, and a
	// policy that rejected the wrong casing would refuse a correctly secured
	// file.
	assert.NoError(t, CheckGrants(`C:\x`, []Grant{{SID: "s-1-5-18", CanWrite: true}}))
}

func TestCheckGrants_ShouldNameEveryOffenderAndTheFix(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckGrants(`C:\cfg\tokens.toml`, []Grant{
		{SID: "S-1-5-32-545", CanWrite: true},
		{SID: "S-1-1-0", CanWrite: true},
	})

	// ASSERT
	// Both, and the command that repairs it: the message is what an operator
	// acts on, and one offender at a time is the version that costs an
	// afternoon.
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "S-1-5-32-545")
	assert.Contains(t, msg, "S-1-1-0")
	assert.Contains(t, msg, `C:\cfg\tokens.toml`)
	assert.Contains(t, msg, "service secure")
}
