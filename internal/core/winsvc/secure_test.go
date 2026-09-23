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
	CheckSecurity    -> - TestCheckSecurity_ShouldAcceptThePolicyGrantees
	                    - TestCheckSecurity_ShouldRejectAnOutsideWrite
	                    - TestCheckSecurity_ShouldIgnoreAnOutsideRead
	                    - TestCheckSecurity_ShouldMatchSIDsCaseInsensitively
	                    - TestCheckSecurity_ShouldNameEveryOffenderAndTheFix
	                    - TestCheckSecurity_ShouldAllowCreatorOwner
	                    - TestCheckSecurity_ShouldNameEachOffenderOnce
	                    - TestCheckSecurity_ShouldRejectAnOwnerOutsideThePolicy
	                    - TestCheckSecurity_ShouldIgnoreTheOwnerWhenOnlyForeignWriteIsAsked
	                    - TestCheckSecurity_ShouldRejectAnUnownedDescriptor
	CheckNotReparsePoint -> - TestCheckNotReparsePoint_ShouldAcceptARealDirectoryAndAnAbsentPath
	                        - TestCheckNotReparsePoint_ShouldRefuseALinkStandingInForADirectory
	IsProtectedLocation -> - TestIsProtectedLocation_ShouldRefuseVolumeRootsAndSharedSystemDirectories:
	                         including the \\?\ and \\.\ spellings, which
	                         IsAbsoluteServicePath accepts and which read as UNC
	                         paths unless the prefix is stripped first.
	                       - TestIsProtectedLocation_ShouldAcceptADirectoryOfTheAdaptersOwn
	CheckSecurables  -> - TestCheckSecurables_ShouldRefuseADirectoryTargetAtAProtectedLocation
	                    - TestCheckSecurables_ShouldAllowAFileAtAProtectedLocation
	                    - TestCheckSecurables_ShouldAllowTheDefaultLayout

Tested elsewhere:

	Applying the policy, and reading a real owner and access list back:
	secure_windows.go against a live filesystem, exercised by task service-gate.
	The apply verifies its own result through CheckSecurity, so the function
	tested here is the one that decides whether a lockdown took.

	That install and `service secure` call it with the resolved paths:
	cmd/weave-adapter-dhcp-windows/service_test.go.

	That the binary fails closed on a descriptor it cannot read:
	cmd/weave-adapter-dhcp-windows/main_test.go and service_test.go, which own
	that decision — this package only supplies the read.

Declined:

	Testing SetNamedSecurityInfo. It mutates a real security descriptor and
	needs Administrator; a unit test would either fail on the unprivileged
	ci:windows runner or leave a re-owned file behind.

	Creating a real junction for CheckNotReparsePoint. It needs Windows and a
	privilege the ci:windows runner does not hold. A symlink exercises the same
	expression: since Go 1.23 a junction reports ModeIrregular and a symlink
	ModeSymlink, and the check tests for both.

Additional Remarks:

	The policy is portable data on purpose. It replaces
	scripts/secure-token-store.ps1, whose defaults had drifted from the
	milestone's decisions -- it granted NETWORK SERVICE, defaulted to a
	relative path Phase 3 now rejects, and secured files only, so a log file
	created later inherited nothing from it.

	Only writes are refused, not reads. The token store holds hashes, which
	cannot be replayed, and a rule that failed on a benign backup or auditing
	read entry is a rule that gets switched off.

	Ownership is checked under PolicyOwned only, and the split is not
	fastidiousness: %ProgramFiles% is owned by TrustedInstaller and a
	subdirectory of it by whichever elevated operator created it, so a single
	strict rule would refuse every correct install of the binary. What Secure
	touches, Secure owns — it sets the owner to Administrators as part of
	applying the list — so the strict question is answerable exactly where it
	is asked.
*/
package winsvc

import (
	"os"
	"path/filepath"
	"strings"
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
	// A literal Windows path, and both separators, because the derivation is
	// Windows' rather than the host's: filepath.Dir off Windows sees no
	// separator in this at all.
	tests := map[string]string{
		`C:\logs\adapter.log`: `C:\logs`,
		`C:/logs/adapter.log`: `C:\logs`,
		`C:\adapter.log`:      `C:\`,
		`\\ws2022\logs\a.log`: `\\ws2022\logs`,
	}

	for logFile, wantDir := range tests {
		// ACT
		got := SecurablesFor(`C:\cfg\config.toml`, "", logFile)

		// ASSERT
		// The adapter creates the log itself, at runtime, and a new file takes
		// its parent's inheritable entries. Securing the file would leave the
		// next one created under whatever the directory permits.
		require.Len(t, got, 2)
		assert.Equal(t, wantDir, got[1].Path)
	}

	got := SecurablesFor(`C:\cfg\config.toml`, "", `C:\logs\adapter.log`)
	require.Len(t, got, 2)
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

func TestCheckSecurity_ShouldAcceptThePolicyGrantees(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.NoError(t, CheckSecurity(`C:\cfg\tokens.toml`, Security{
		Owner: SIDAdministrators,
		Grants: []Grant{
			{SID: SIDLocalSystem, CanWrite: true},
			{SID: SIDAdministrators, CanWrite: true},
		},
	}, PolicyOwned))
}

func TestCheckSecurity_ShouldRejectAnOutsideWrite(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// S-1-5-32-545 is Users. A write here is a local privilege escalation into
	// the API: the adapter trusts every hash in the store it reads at startup.
	sec := Security{Owner: SIDAdministrators, Grants: []Grant{
		{SID: SIDLocalSystem, CanWrite: true},
		{SID: "S-1-5-32-545", CanWrite: true},
	}}

	// ACT
	err := CheckSecurity(`C:\cfg\tokens.toml`, sec, PolicyOwned)

	// ASSERT
	require.ErrorIs(t, err, ErrNotSecured)
	assert.Contains(t, err.Error(), "S-1-5-32-545")
}

func TestCheckSecurity_ShouldIgnoreAnOutsideRead(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckSecurity(`C:\cfg\tokens.toml`, Security{
		Owner: SIDAdministrators,
		Grants: []Grant{
			{SID: SIDLocalSystem, CanWrite: true},
			{SID: "S-1-5-32-545", CanWrite: false},
		},
	}, PolicyOwned)

	// ASSERT
	// A read of the store leaks hashes, which cannot be replayed as tokens,
	// and failing on a benign backup or auditing entry would make this a rule
	// operators switch off rather than satisfy.
	assert.NoError(t, err)
}

func TestCheckSecurity_ShouldMatchSIDsCaseInsensitively(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	// Windows renders a SID string in upper case but accepts either, and a
	// policy that rejected the wrong casing would refuse a correctly secured
	// file. It binds to the owner as well as to the grantees.
	assert.NoError(t, CheckSecurity(`C:\x`, Security{
		Owner:  "s-1-5-32-544",
		Grants: []Grant{{SID: "s-1-5-18", CanWrite: true}},
	}, PolicyOwned))
}

func TestCheckSecurity_ShouldNameEveryOffenderAndTheFix(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckSecurity(`C:\cfg\tokens.toml`, Security{
		Owner: SIDAdministrators,
		Grants: []Grant{
			{SID: "S-1-5-32-545", CanWrite: true},
			{SID: "S-1-1-0", CanWrite: true},
		},
	}, PolicyOwned)

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

func TestSecureResult_ShouldDistinguishAppliedFromSkipped(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The token store is the one legitimately-absent target: an operator can
	// install before minting a token.
	targets := SecurablesFor(`C:\cfg\config.toml`, `C:\cfg\tokens.toml`, "")

	// ACT
	applied := SecureResult{Target: targets[0], Applied: true}
	skipped := SecureResult{Target: targets[1], Applied: false}

	// ASSERT
	// The distinction exists so the installer cannot print "secured" for a
	// file it never touched. An operator who then runs `token gen` would
	// believe the store is protected when it inherited the directory default
	// — which is precisely the escalation the lockdown exists to close.
	assert.True(t, applied.Applied)
	assert.False(t, skipped.Applied)
	assert.True(t, skipped.Target.Optional, "only an optional target may be skipped")
}

func TestCheckSecurity_ShouldAllowCreatorOwner(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckSecurity(`C:\Program Files\weave-adapters`, Security{
		Owner: SIDAdministrators,
		Grants: []Grant{
			{SID: SIDLocalSystem, CanWrite: true},
			{SID: SIDAdministrators, CanWrite: true},
			{SID: SIDCreatorOwner, CanWrite: true},
		},
	}, PolicyOwned)

	// ASSERT
	// CREATOR OWNER is on nearly every standard Windows location, C:\Program
	// Files included — which is exactly where a service binary belongs.
	// Treating it as an offender would refuse the correct install location.
	// It is a template rather than a principal: nobody who cannot already
	// create a file there ever becomes a creator-owner.
	assert.NoError(t, err)
}

func TestCheckSecurity_ShouldNameEachOffenderOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// Windows commonly carries two entries for one principal — an inherit-only
	// one and an effective one. The first real run reported
	// "[S-1-5-32-545 S-1-5-32-545 S-1-3-0]", which reads as three problems and
	// is one.
	sec := Security{Owner: SIDAdministrators, Grants: []Grant{
		{SID: "S-1-5-32-545", CanWrite: true},
		{SID: "S-1-5-32-545", CanWrite: true},
		{SID: SIDCreatorOwner, CanWrite: true},
	}}

	// ACT
	err := CheckSecurity(`C:\gate\bin`, sec, PolicyOwned)

	// ASSERT
	require.ErrorIs(t, err, ErrNotSecured)
	assert.Equal(t, 1, strings.Count(err.Error(), "S-1-5-32-545"))
	assert.NotContains(t, err.Error(), SIDCreatorOwner)
}

func TestCheckSecurity_ShouldRejectAnOwnerOutsideThePolicy(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The access list is exactly what the lockdown applies. What is wrong is
	// who owns it: an owner holds READ_CONTROL and WRITE_DAC implicitly, so
	// this list is one the owner can replace whenever they like.
	//
	// S-1-5-21-…-1001 is an ordinary local account — the shape of a user who
	// pre-created C:\ProgramData\weave-adapters, which C:\ProgramData lets any
	// authenticated account do.
	sec := Security{
		Owner: "S-1-5-21-1111111111-2222222222-3333333333-1001",
		Grants: []Grant{
			{SID: SIDLocalSystem, CanWrite: true},
			{SID: SIDAdministrators, CanWrite: true},
		},
	}

	// ACT
	err := CheckSecurity(`C:\ProgramData\weave-adapters`, sec, PolicyOwned)

	// ASSERT
	require.ErrorIs(t, err, ErrNotOwned)
	assert.Contains(t, err.Error(), "S-1-5-21-1111111111-2222222222-3333333333-1001")
	assert.Contains(t, err.Error(), "service secure")
}

func TestCheckSecurity_ShouldIgnoreTheOwnerWhenOnlyForeignWriteIsAsked(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// The binary's directory. %ProgramFiles% is owned by TrustedInstaller and
	// a subdirectory of it by whichever elevated operator created it, under
	// Windows' default "object creator" owner policy — so demanding SYSTEM or
	// Administrators here would refuse every correct install.
	sec := Security{
		Owner: "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464",
		Grants: []Grant{
			{SID: SIDLocalSystem, CanWrite: true},
			{SID: SIDAdministrators, CanWrite: true},
			{SID: SIDCreatorOwner, CanWrite: true},
		},
	}

	// ACT / ASSERT
	require.NoError(t, CheckSecurity(`C:\Program Files\weave-adapters`, sec, PolicyNoForeignWrite))
	require.ErrorIs(t, CheckSecurity(`C:\Program Files\weave-adapters`, sec, PolicyOwned), ErrNotOwned)
}

func TestCheckSecurity_ShouldRejectAnUnownedDescriptor(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// GetSecurityDescriptorOwner succeeds and hands back nil for a descriptor
	// carrying no owner. Refused rather than passed: "nobody owns this" and
	// "somebody we would refuse owns this" are the same answer downstream.
	sec := Security{Grants: []Grant{{SID: SIDLocalSystem, CanWrite: true}}}

	// ACT / ASSERT
	require.ErrorIs(t, CheckSecurity(`C:\cfg`, sec, PolicyOwned), ErrNotOwned)
	require.NoError(t, CheckSecurity(`C:\cfg`, sec, PolicyNoForeignWrite))
}

func TestCheckNotReparsePoint_ShouldAcceptARealDirectoryAndAnAbsentPath(t *testing.T) {
	t.Parallel()

	// ARRANGE
	dir := t.TempDir()

	// ACT / ASSERT
	assert.NoError(t, CheckNotReparsePoint(dir))

	// Absent is fine: the caller is about to create it, and a name that does
	// not exist yet cannot redirect anything.
	assert.NoError(t, CheckNotReparsePoint(filepath.Join(dir, "not-there")))
}

func TestCheckNotReparsePoint_ShouldRefuseALinkStandingInForADirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// A symlink here, because that is the reparse point a non-Windows host can
	// create; on Windows the shape that matters is a junction, which os.Lstat
	// reports as ModeIrregular and this same expression catches.
	base := t.TempDir()
	target := filepath.Join(base, "attacker")
	link := filepath.Join(base, "weave-adapters")

	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.Symlink(target, link))

	// ACT
	err := CheckNotReparsePoint(link)

	// ASSERT
	// Everything downstream — os.Stat, SetNamedSecurityInfo,
	// GetNamedSecurityInfo — resolves the path by name and lands on the
	// target, so a lockdown applied here would secure the attacker's directory
	// and the config would be written through it.
	require.ErrorIs(t, err, ErrReparsePoint)
	assert.Contains(t, err.Error(), link)
}

func TestIsProtectedLocation_ShouldRefuseVolumeRootsAndSharedSystemDirectories(t *testing.T) {
	t.Parallel()

	// ARRANGE
	protected := []string{
		`C:\`, `C:`, `c:/`, `D:\`,
		`C:\Windows`, `C:\WINDOWS\`, `C:\Windows\System32`,
		`C:\ProgramData`, `c:\programdata\`,
		`C:\Program Files`, `C:\Program Files (x86)`,
		`C:\Users`, `C:\Users\Public`,
		`\\fileserver`, `\\fileserver\share`, `\\fileserver\share\`,

		// The extended-length and device spellings of the same directories.
		// config.IsAbsoluteServicePath accepts them — they begin with two
		// separators — so without the prefix strip they read as UNC paths with
		// enough segments to fall through every rule.
		`\\?\C:\ProgramData`, `\\?\C:\`, `\\?\c:\program files`, `\\.\C:\Windows`,
	}

	// ACT / ASSERT
	for _, path := range protected {
		assert.True(t, IsProtectedLocation(path), "%s must be refused", path)
	}
}

func TestIsProtectedLocation_ShouldAcceptADirectoryOfTheAdaptersOwn(t *testing.T) {
	t.Parallel()

	// ARRANGE
	ours := []string{
		`C:\ProgramData\weave-adapters`,
		`C:\Program Files\weave-adapters`,
		`C:\Users\Public\wadapt`,
		`\\fileserver\share\weave-adapters`,
		`D:\wadapt`,
		`\\?\C:\ProgramData\weave-adapters`,
		"",
	}

	// ACT / ASSERT
	for _, path := range ours {
		assert.False(t, IsProtectedLocation(path), "%s must be allowed", path)
	}
}

func TestCheckSecurables_ShouldRefuseADirectoryTargetAtAProtectedLocation(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// `logFile = C:\adapter.log` is all it takes: SecurablesFor secures the
	// log file's DIRECTORY, and the directory of a file at the volume root is
	// the volume root. CheckServicePaths passes it, because it only asks
	// whether a path is absolute.
	targets := SecurablesFor(`C:\wadapt\config.toml`, "", `C:\adapter.log`)

	// ACT
	err := CheckSecurables(targets)

	// ASSERT
	// The entries the lockdown applies are protected and inheriting, so this
	// would detach the whole volume from its inherited grants and strip every
	// other application's access to its own files. It needs an administrator,
	// so it is a typo rather than an attack — and it is not undone by
	// re-running anything.
	require.ErrorIs(t, err, ErrProtectedLocation)
	assert.Contains(t, err.Error(), "the log directory")
}

func TestCheckSecurables_ShouldAllowAFileAtAProtectedLocation(t *testing.T) {
	t.Parallel()

	// ARRANGE
	// Directories only. A config file sitting at a volume root is odd, but the
	// lockdown applied to a FILE inherits to nothing — it protects that file
	// and nothing else.
	targets := SecurablesFor(`C:\config.toml`, `C:\tokens.toml`, "")

	// ACT / ASSERT
	assert.NoError(t, CheckSecurables(targets))
}

func TestCheckSecurables_ShouldAllowTheDefaultLayout(t *testing.T) {
	t.Parallel()

	// ARRANGE
	targets := SecurablesFor(
		`C:\ProgramData\weave-adapters\config.toml`,
		`C:\ProgramData\weave-adapters\tokens.toml`,
		`C:\ProgramData\weave-adapters\adapter.log`,
	)

	// ACT / ASSERT
	// The rule has to pass what the tool itself provisions, or it is a rule
	// that gets deleted rather than satisfied.
	assert.NoError(t, CheckSecurables(targets))
}
