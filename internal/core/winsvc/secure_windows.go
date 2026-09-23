//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dirMode is the mode a created log directory takes before the ACL replaces
// it. Windows ignores the bits entirely; the protected DACL applied a moment
// later is the real protection. It is here rather than beside the policy
// because nothing off Windows creates anything.
const dirMode = 0o700

// Secure applies the lockdown to every target and verifies it took.
//
// It needs Administrator: replacing an owner requires the admin token, which
// `service install` already demands for the SCM anyway.
//
// Targets marked Optional are skipped when absent. The log file is configured
// at install time and created at first run, so its directory is secured and
// the file inherits.
func Secure(targets []Securable) ([]SecureResult, error) {
	var (
		errs    []error
		results = make([]SecureResult, 0, len(targets))
	)

	for _, t := range targets {
		applied, err := secureOne(t)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		results = append(results, SecureResult{Target: t, Applied: applied})
	}

	return results, errors.Join(errs...)
}

// secureOne applies the policy to a single target, then reads it back. It
// reports whether anything was applied — an absent optional target is skipped,
// and the caller must be able to say so rather than claim it was secured.
func secureOne(t Securable) (bool, error) {
	// Before anything, including MkdirAll. A junction already at t.Path makes
	// every call below act on its target: MkdirAll succeeds, Stat follows it,
	// and SetNamedSecurityInfo locks down a directory somebody else owns and
	// can re-point afterwards.
	if err := CheckNotReparsePoint(t.Path); err != nil {
		return false, err
	}

	if err := CheckSecurables([]Securable{t}); err != nil {
		return false, err
	}

	if t.Kind == SecurableDirectory {
		// Created rather than demanded. Install is elevated and holds the
		// resolved path, and failing here would leave a REGISTERED service
		// with an ENOENT wrapped in "securing failed" — which is the worst
		// moment to discover that a directory in the config does not exist
		// yet.
		if err := os.MkdirAll(t.Path, dirMode); err != nil {
			return false, fmt.Errorf("creating %s (%s): %w", t.Path, t.Why, err)
		}
	}

	if _, err := os.Stat(t.Path); err != nil {
		if t.Optional && errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("securing %s (%s): %w", t.Path, t.Why, err)
	}

	dacl, err := lockdownDACL(t.Inheritance())
	if err != nil {
		return false, fmt.Errorf("building the access list for %s: %w", t.Path, err)
	}

	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("resolving the administrators group: %w", err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION is what makes this a lockdown rather
	// than an addition: it detaches the object from its parent's inheritable
	// entries, which are exactly the Users:Modify grants being replaced.
	// Without it the new entries are merged with the inherited ones and the
	// wider grant survives.
	err = windows.SetNamedSecurityInfo(
		t.Path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION|
			windows.OWNER_SECURITY_INFORMATION,
		admins,
		nil,
		dacl,
		nil,
	)
	if err != nil {
		return false, fmt.Errorf("securing %s (%s) — this needs an elevated prompt: %w", t.Path, t.Why, err)
	}

	// Read back, always. An apply that reported success and left the object
	// wide open would be worse than no lockdown, because the install output
	// would say it was secured. This is also the verifier the startup check
	// uses, so there is one implementation of "is this locked down" rather
	// than a checker that has to agree with an applier.
	sec, err := ReadSecurity(t.Path)
	if err != nil {
		return false, fmt.Errorf("verifying %s: %w", t.Path, err)
	}

	// PolicyOwned: the call above set the owner as well as the list, so the
	// read-back is what proves BOTH took. Verifying only the list would report
	// success for an object whose owner was never replaced — and an owner
	// holds WRITE_DAC implicitly, so it could undo the list at any moment.
	if err := CheckSecurity(t.Path, sec, PolicyOwned); err != nil {
		return false, fmt.Errorf("the lockdown did not take on %s (%s): %w", t.Path, t.Why, err)
	}

	return true, nil
}

// fileAllAccess is FILE_ALL_ACCESS, which x/sys does not define.
//
// Used rather than GENERIC_ALL, which also works: Get-Acl renders a generic
// mask as the raw number 268435456 while this renders as FullControl in every
// tool an operator or a gate will use. How generic bits map onto an inherited
// entry is also the system's business rather than ours, and a specific mask
// leaves nothing to interpret.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1FF

// lockdownDACL builds the access list: full control for SYSTEM and
// Administrators, and no other entry at all.
func lockdownDACL(inherit Inheritance) (*windows.ACL, error) {
	flags := uint32(windows.NO_INHERITANCE)
	if inherit == InheritToChildren {
		// Both flags, or a new log file created in the directory inherits
		// nothing and lands on the parent's defaults.
		flags = windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(LockdownGrantees()))

	for _, s := range LockdownGrantees() {
		sid, err := windows.StringToSid(s)
		if err != nil {
			return nil, fmt.Errorf("parsing the SID %s: %w", s, err)
		}

		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       flags,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}

	// nil merged ACL: the new list replaces rather than extends.
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("assembling the access list: %w", err)
	}

	return acl, nil
}

// ReadSecurity reads a path's owner and access-control entries into the
// portable shape CheckSecurity understands.
//
// The owner is requested alongside the list, and that is the point: an owner
// holds READ_CONTROL and WRITE_DAC implicitly, so an access list read without
// one describes a lock whose key may be in somebody else's pocket. Asking for
// both in one call is also what keeps them consistent — two calls could
// straddle a change.
//
// Only allow entries are reported. A deny entry narrows access, and this check
// is about who has more than the policy permits.
func ReadSecurity(path string) (Security, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Security{}, fmt.Errorf("reading the security descriptor of %q: %w", path, err)
	}

	owner, _, err := sd.Owner()
	if err != nil {
		return Security{}, fmt.Errorf("reading the owner of %q: %w", path, err)
	}

	// A descriptor may carry no owner at all, in which case the call above
	// succeeds and hands back nil. Reported as an empty owner rather than
	// papered over: CheckSecurity refuses that under PolicyOwned, which is the
	// right answer for an object nobody is recorded as owning.
	var ownerSID string
	if owner != nil {
		ownerSID = owner.String()
	}

	grants, err := readGrants(path, sd)
	if err != nil {
		return Security{}, err
	}

	return Security{Owner: ownerSID, Grants: grants}, nil
}

// readGrants pulls the allow entries out of a descriptor already read.
func readGrants(path string, sd *windows.SECURITY_DESCRIPTOR) ([]Grant, error) {
	dacl, _, err := sd.DACL()
	if err != nil {
		// The DACL-present bit is clear. Reported as wide open rather than as
		// an error: an object with no access list is one nothing restricts.
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return everyoneWrites(), nil
		}

		return nil, fmt.Errorf("reading the access list of %q: %w", path, err)
	}

	// PRESENT AND NIL is the canonical NULL DACL — the state that grants
	// everyone full access — and DACL() returns it with a nil error, so this
	// is not the branch above. Dereferencing here would panic, and it would
	// panic inside the service's startup check, before any log is open: the
	// SCM would report a process that died with no explanation anywhere.
	if dacl == nil {
		return everyoneWrites(), nil
	}

	out := make([]Grant, 0, dacl.AceCount)

	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE

		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, fmt.Errorf("reading entry %d of %q: %w", i, path, err)
		}

		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}

		// The SID is stored inline at the end of the ACE rather than as a
		// pointer, so reading it means taking the address of that trailing
		// field. This is the documented layout of ACCESS_ALLOWED_ACE and the
		// only way x/sys exposes it.
		//nolint:gosec // G103: the documented inline SID of an ACE returned by GetAce.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))

		out = append(out, Grant{SID: sid.String(), CanWrite: confersWrite(ace.Mask)})
	}

	return out, nil
}

// everyoneWrites is what a NULL DACL means: no access list at all, so every
// principal has full control. S-1-1-0 is Everyone.
func everyoneWrites() []Grant {
	return []Grant{{SID: "S-1-1-0", CanWrite: true}}
}

// writeMask is every right that lets a holder change the object or its
// contents. Taking the whole set rather than GENERIC_WRITE alone matters:
// FILE_WRITE_DATA on the token store is the escalation, and it is granted
// independently of the generic bit.
//
// FILE_DELETE_CHILD is spelled out below because x/sys does not define it. On
// a DIRECTORY it lets its holder delete or rename any child whatever the
// child's own list says, so a principal holding only that on the data
// directory can remove the token store or the executable — not a replacement,
// but a clean denial of service, and it is granted independently of DELETE.
// The bit has no meaning on a file, so including it costs nothing there.
const fileDeleteChild = 0x00000040

const writeMask = windows.GENERIC_ALL |
	fileDeleteChild |
	windows.GENERIC_WRITE |
	windows.WRITE_OWNER |
	windows.WRITE_DAC |
	windows.DELETE |
	windows.FILE_WRITE_DATA |
	windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_ATTRIBUTES |
	windows.FILE_WRITE_EA

// confersWrite reports whether an access mask lets its holder change anything.
func confersWrite(mask windows.ACCESS_MASK) bool {
	return uint32(mask)&uint32(writeMask) != 0
}
