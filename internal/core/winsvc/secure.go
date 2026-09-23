package winsvc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Well-known SIDs the lockdown grants, as strings so the policy below is
// portable data rather than a Windows API call.
//
// SIDs, never names. A name is locale-dependent — on a German host the local
// administrators group is "Administratoren" — so a policy written in names
// would apply correctly on an English host and silently fail to match
// anywhere else. `scripts/secure-token-store.ps1` learned this first and its
// reasoning carries over unchanged.
const (
	// SIDLocalSystem is the account the service runs as.
	//
	// S-1-5-18, not the S-1-5-20 (NETWORK SERVICE) the old script defaulted
	// to: that default predates the LocalSystem decision, which was forced by
	// measurement — the PowerShell DHCP cmdlets gate on Administrators, and
	// NETWORK SERVICE was refused WIN32 5 on WS2022 in every group tried.
	SIDLocalSystem = "S-1-5-18"

	// SIDAdministrators is the local Administrators group, so an operator can
	// still read and repair what the service owns.
	SIDAdministrators = "S-1-5-32-544"
)

// Inheritance describes how an entry propagates to children. It mirrors the
// Windows flags without importing them, so the policy stays portable.
type Inheritance int

const (
	// InheritNone applies to the object itself only — the shape a single file
	// needs.
	InheritNone Inheritance = iota

	// InheritToChildren applies to the container and everything created
	// inside it.
	//
	// The log directory needs this and a file-only grant would not do: the
	// adapter creates the log itself, at runtime, and a new file inherits its
	// parent's inheritable entries. Without them the log would be created
	// under whatever the directory's default permits, which is the ACL the
	// lockdown was replacing.
	InheritToChildren
)

// SecurableKind distinguishes the two shapes a target can have.
type SecurableKind int

const (
	// SecurableFile is a single file.
	SecurableFile SecurableKind = iota
	// SecurableDirectory is a directory and everything created in it.
	SecurableDirectory
)

// Securable is one thing the lockdown applies to.
type Securable struct {
	// Path is the absolute path to the file or directory.
	Path string
	// Kind selects file or directory semantics.
	Kind SecurableKind
	// Why names the target for the operator-facing message. It is not
	// decoration: a failed lockdown has to say which of three paths failed and
	// what is at risk there.
	Why string
	// Optional marks a target that need not exist. The log file is configured
	// but not yet created at install time.
	Optional bool
}

// Inheritance reports how entries on this target should propagate.
func (s Securable) Inheritance() Inheritance {
	if s.Kind == SecurableDirectory {
		return InheritToChildren
	}

	return InheritNone
}

// LockdownGrantees is who the lockdown grants full control to, and nobody
// else gets anything.
//
// Two principals, not three. The old script also granted the service account
// separately; with LocalSystem that account *is* S-1-5-18, so the third grant
// collapsed into the first when the decision landed.
func LockdownGrantees() []string {
	return []string{SIDLocalSystem, SIDAdministrators}
}

// SecurablesFor returns everything an installed service needs locked down.
//
// The binary supplies the paths because it is the one that just resolved and
// validated them — the same reason the installer is a Go subcommand rather
// than a script. A script would have to be told all three and would drift
// from them silently.
func SecurablesFor(configPath, tokenStore, logFile string) []Securable {
	out := []Securable{{
		Path: configPath,
		Kind: SecurableFile,
		// It carries identity.namespaceKey. A read leaks the value every
		// wadaptID on this host derives from; a write re-keys the fleet.
		Why: "the config file, which holds identity.namespaceKey",
	}}

	if tokenStore != "" {
		out = append(out, Securable{
			Path: tokenStore,
			Kind: SecurableFile,
			// A read leaks nothing usable -- the file holds hashes -- but a
			// write is a local privilege escalation into the API: anyone who
			// can append a hash gets a token the adapter accepts.
			Why:      "the bearer token store, where a write grants API access",
			Optional: true,
		})
	}

	if logFile != "" {
		out = append(out, Securable{
			Path: logDirOf(logFile),
			Kind: SecurableDirectory,
			// Not the file: the adapter creates it, so the directory's
			// inheritable entries are what decide the new file's ACL.
			Why: "the log directory, whose entries the log file inherits",
		})
	}

	return out
}

// SecureResult reports what happened to one target.
//
// Applied distinguishes "locked down" from "skipped because it is not there
// yet", which the token store legitimately is before the first `token gen`.
// Reporting a skipped target as secured is worse than saying nothing: an
// operator who mints a token afterwards would believe a file is protected
// when it inherited the directory's defaults.
type SecureResult struct {
	Target  Securable
	Applied bool
}

// ErrNotSecured reports that a target's permissions are wider than the policy
// allows.
var ErrNotSecured = errors.New("winsvc: the path grants access beyond SYSTEM and Administrators")

// ErrNotOwned reports that a target is owned by a principal outside the
// policy.
//
// Its own error rather than a flavour of ErrNotSecured, because it is a
// different failure with a different fix: an access list can be re-applied,
// while an object somebody else owns has to be taken over before anything
// applied to it means anything.
var ErrNotOwned = errors.New("winsvc: the path is owned by a principal outside SYSTEM and Administrators")

// ErrReparsePoint reports a path that redirects somewhere else — a symlink, a
// junction, or any other reparse point.
var ErrReparsePoint = errors.New("winsvc: the path is a reparse point and would redirect this operation")

// ErrProtectedLocation reports a lockdown aimed at a volume root or a shared
// system directory.
var ErrProtectedLocation = errors.New("winsvc: the path is a volume root or a shared system directory")

// Grant is one access-control entry read back from a target.
type Grant struct {
	// SID is the trustee, as a string.
	SID string
	// CanWrite reports whether the entry confers any kind of write.
	CanWrite bool
}

// Security is a securable read back from disk: who owns it, and who it grants
// access to.
//
// The owner is not bookkeeping. On Windows an owner holds READ_CONTROL and
// WRITE_DAC implicitly, whatever the access list says — so a check that reads
// only the list answers "locked down" for an object whose owner can re-open it
// at will. That is exactly what a directory pre-created under C:\ProgramData
// by an unprivileged user is: any authenticated account may create one there,
// and the creator owns it.
type Security struct {
	// Owner is the owning principal's SID, as a string.
	Owner string
	// Grants are the allow entries on the object.
	Grants []Grant
}

// Policy says how strictly a securable is judged.
//
// Two policies rather than one, because the two questions this package is
// asked are genuinely different and the strict answer is wrong for one of
// them.
type Policy int

const (
	// PolicyNoForeignWrite refuses only an access entry granting write to a
	// principal outside the policy. Ownership is deliberately not part of it.
	//
	// It is the question asked of the directory the SERVICE BINARY runs from,
	// which this tool checks and never repairs. That directory is %ProgramFiles%
	// or a subdirectory of it: %ProgramFiles% itself is owned by
	// NT SERVICE\TrustedInstaller, and a subdirectory created by an elevated
	// operator is owned by that operator under Windows' default owner policy
	// ("Object creator", the shipping default since Server 2003). Demanding
	// SYSTEM or Administrators there would refuse every correct install.
	PolicyNoForeignWrite Policy = iota

	// PolicyOwned also refuses an owner outside the policy.
	//
	// It is the question asked of everything this tool LOCKS DOWN ITSELF —
	// the provisioning directory, the config file, the token store, the log
	// directory. Secure sets the owner to Administrators as part of applying
	// the list, so any target it has touched answers this; a target that does
	// not is one the lockdown never reached, or one somebody else created
	// first and can silently re-open.
	PolicyOwned
)

// CheckSecurity reports whether a securable read back from disk stays within
// the policy.
//
// One entry point rather than an owner check beside a grants check, so a
// caller cannot ask the weaker question by accident — which is how the owner
// came to be set on every lockdown and read back by nothing.
func CheckSecurity(path string, sec Security, policy Policy) error {
	if policy == PolicyOwned {
		if err := checkOwner(path, sec.Owner); err != nil {
			return err
		}
	}

	return checkGrants(path, sec.Grants)
}

// checkOwner reports an owner outside the policy.
//
// LockdownGrantees rather than a second list: the two sets coincide by
// construction, since Secure grants exactly SYSTEM and Administrators and sets
// the owner to Administrators. SIDCreatorOwner is NOT added here, unlike in
// checkGrants — S-1-3-0 is a template applied as an object is created, never a
// principal that ends up owning one, so an object reporting it as its owner is
// an object nothing sane produced.
func checkOwner(path, owner string) error {
	if owner == "" {
		// No owner read back at all. Refused rather than passed: the whole
		// point of reading it is that an unowned answer and a wrongly-owned
		// one are indistinguishable to everything downstream.
		return fmt.Errorf("%w: %s reports no owner", ErrNotOwned, path)
	}

	if containsSID(LockdownGrantees(), owner) {
		return nil
	}

	return fmt.Errorf("%w: %s is owned by %s, which holds WRITE_DAC on it whatever the access list says; "+
		"run `weave-adapter-dhcp-windows service secure` from an elevated prompt to take it over",
		ErrNotOwned, path, owner)
}

// SIDCreatorOwner is the CREATOR OWNER placeholder.
//
// Allowed wherever it appears as a GRANTEE, and it appears on nearly every
// standard Windows location — including C:\Program Files, which is exactly
// where a service binary should live. It is not a principal: it is a template
// saying "whoever creates an object here owns it", applied as the object is
// created. Nobody who cannot already create a file there ever becomes a
// creator-owner, so on its own it grants no access to anyone. Treating it as
// an offender would refuse the correct install location.
const SIDCreatorOwner = "S-1-3-0"

// checkGrants reports whether the entries read from a target stay within the
// policy.
//
// Only writes are refused. A read of the token store leaks hashes, which
// cannot be replayed, and refusing every stray read entry would make the check
// fail on hosts with a benign auditing or backup grant — a rule nobody could
// satisfy is a rule that gets disabled. A write is the actual escalation, on
// all three targets.
func checkGrants(path string, grants []Grant) error {
	allowed := append(LockdownGrantees(), SIDCreatorOwner)

	var offenders []string

	for _, g := range grants {
		if !g.CanWrite {
			continue
		}

		if containsSID(allowed, g.SID) || containsSID(offenders, g.SID) {
			// Deduplicated: Windows commonly carries two entries for one
			// principal — an inherit-only one and an effective one — and
			// naming it twice in the error reads like two problems.
			continue
		}

		offenders = append(offenders, g.SID)
	}

	if len(offenders) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s grants write to %v; run `weave-adapter-dhcp-windows service secure` "+
		"from an elevated prompt", ErrNotSecured, path, offenders)
}

// CheckNotReparsePoint refuses a path that redirects somewhere else.
//
// It is the other half of the ownership check, and without it the ownership
// check can be walked around. Every path here is resolved by NAME and every
// operation on it follows a reparse point: os.Stat, SetNamedSecurityInfo and
// GetNamedSecurityInfo all land on the TARGET. So an unprivileged user who
// creates C:\ProgramData\weave-adapters as a junction before setup runs — and
// C:\ProgramData admits any authenticated account to create a subdirectory —
// has the whole provisioning sequence write into a directory of their own,
// which they can re-point afterwards.
//
// Lstat, not Stat, because Stat is the thing being defended against. An absent
// path is fine: the caller is about to create it, and creating through a name
// that does not exist yet cannot be redirected.
//
// Portable rather than platform-split, and the mode bits are why: since Go
// 1.23 a Windows junction reports ModeIrregular and a symlink ModeSymlink, so
// the two together name every name-surrogate reparse point without a syscall
// — and the same expression catches a symlink on the developer host this
// package is iterated on.
func CheckNotReparsePoint(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("reading %q: %w", path, err)
	}

	if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s; everything here resolves it by name and would act on whatever it points at. "+
		"Remove it and let this run create a real directory", ErrReparsePoint, path)
}

// CheckSecurables refuses a lockdown that would be applied somewhere shared.
//
// It runs before Secure rather than inside it because the damage is not
// undone by noticing it afterwards: the entries the lockdown applies are
// PROTECTED and INHERITING, so aiming one at C:\ or C:\ProgramData detaches
// that whole subtree from its inherited grants and strips every other
// application's access to its own data. An administrator has to run it, so it
// is not attacker-reachable — it is what `--data-dir C:\ProgramData` or
// `logFile = C:\adapter.log` does by typo, and a release pipeline that makes
// provisioning routine is exactly when a typo gets run.
//
// Directories only. A file at a volume root is odd but harmless: the lockdown
// applies to that file and inherits to nothing.
func CheckSecurables(targets []Securable) error {
	var errs []error

	for _, t := range targets {
		if t.Kind != SecurableDirectory {
			continue
		}

		if IsProtectedLocation(t.Path) {
			errs = append(errs, fmt.Errorf(
				"%w: %s (%s). The lockdown replaces the inherited access list of a directory and everything "+
					"created in it, so applying it here would strip every other application's rights to its "+
					"own files. Point it at a directory of this adapter's own",
				ErrProtectedLocation, t.Path, t.Why))
		}
	}

	return errors.Join(errs...)
}

// IsProtectedLocation reports whether a directory is a volume root or a shared
// system directory.
//
// Spelled as data and compared as text, on any host, because it has to be
// testable where nobody can create C:\ProgramData. The environment is read
// where it answers — a host may not have Windows on C: — and the documented
// defaults stand in where it does not, which is every developer machine.
func IsProtectedLocation(path string) bool {
	norm := normalizeLocation(path)
	if norm == "" {
		return false
	}

	// A volume root: "c:" after normalisation, since the trailing separator is
	// trimmed. A service path is absolute by the time it reaches here, so
	// nothing shorter is a real drive.
	if len(norm) == 2 && norm[1] == ':' {
		return true
	}

	// A UNC server or share root, i.e. \\server or \\server\share. The share
	// root is somebody else's whole share; below it is ours to lock.
	if rest, found := strings.CutPrefix(norm, `\\`); found {
		return strings.Count(rest, `\`) < 2
	}

	return slices.Contains(protectedLocations(), norm)
}

// protectedLocations is the list IsProtectedLocation compares against, each
// entry normalised the same way the candidate is.
//
// The hardcoded C: forms sit beside the resolved ones rather than instead of
// them: the resolved ones are what a real host answers, and the literals are
// what a test on a developer machine — and a Windows host with a corrupted
// environment block — compares against.
func protectedLocations() []string {
	out := make([]string, 0, 16)

	for _, v := range []string{"SystemRoot", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "PUBLIC"} {
		if dir := os.Getenv(v); dir != "" {
			out = append(out, normalizeLocation(dir))
		}
	}

	if root := os.Getenv("SystemRoot"); root != "" {
		out = append(out, normalizeLocation(filepath.Join(root, "System32")))
	}

	if drive := os.Getenv("SystemDrive"); drive != "" {
		out = append(out, normalizeLocation(drive+`\Users`))
	}

	return append(out,
		`c:\windows`,
		`c:\windows\system32`,
		`c:\programdata`,
		`c:\program files`,
		`c:\program files (x86)`,
		`c:\users`,
		`c:\users\public`,
	)
}

// normalizeLocation puts a path into the one spelling the comparison uses:
// backslashes, no trailing separator, lower case.
//
// Lower case because Windows paths are case-insensitive and the list would
// otherwise miss `C:\PROGRAMDATA`. strings.ToLower rather than a locale-aware
// fold: every entry in the list is ASCII, and a Turkish locale folding "I" is
// a well-known way to make exactly this kind of comparison stop matching.
func normalizeLocation(path string) string {
	norm := strings.ReplaceAll(strings.TrimSpace(path), "/", `\`)

	// The extended-length and device prefixes are stripped first, or they
	// defeat the whole comparison: `\\?\C:\ProgramData` IS C:\ProgramData, and
	// config.IsAbsoluteServicePath accepts it because it begins with two
	// separators — so without this it read as a UNC path with enough segments
	// to fall through every rule below.
	//
	// Only the drive form is recovered. `\\?\UNC\server\share` re-spells a UNC
	// path and is left alone: it names somebody else's share rather than a
	// system directory, and a rule guessing at it would be a rule nobody can
	// check.
	for _, prefix := range []string{`\\?\`, `\\.\`} {
		if rest, found := strings.CutPrefix(norm, prefix); found && isDriveRooted(rest) {
			norm = rest

			break
		}
	}

	// A UNC prefix is two separators by definition, so the trim has to leave
	// them alone; everything else loses every trailing one.
	if prefix, rest, found := strings.Cut(norm, `\\`); found && prefix == "" {
		return `\\` + strings.ToLower(strings.TrimRight(rest, `\`))
	}

	return strings.ToLower(strings.TrimRight(norm, `\`))
}

// isDriveRooted reports whether a path begins with a drive letter and a colon,
// which is what makes an extended-length prefix safe to strip.
func isDriveRooted(path string) bool {
	return len(path) >= 2 && path[1] == ':' && isASCIILetter(path[0])
}

// isASCIILetter reports whether c names a drive.
func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// containsSID reports whether sid is in the list, case-insensitively: Windows
// renders a SID string in upper case but accepts either.
func containsSID(list []string, sid string) bool {
	return slices.ContainsFunc(list, func(s string) bool {
		return strings.EqualFold(s, sid)
	})
}

// logDirOf returns the directory a log file lives in, under WINDOWS rules.
//
// filepath.Dir is deliberately not used, for the reason
// config.isAbsoluteWindowsPath does not use filepath.IsAbs: it applies the
// HOST's rules. Off Windows it finds no separator in `C:\wadapt\adapter.log`
// at all and answers ".", so the directory of a real logFile is wrong
// everywhere this package is iterated on — and the rule that refuses a
// lockdown aimed at a volume root could not be tested where it is written.
// Encoding the rule here makes it correct where it runs and testable where it
// does not.
func logDirOf(logFile string) string {
	i := strings.LastIndexAny(logFile, `/\`)
	if i < 0 {
		return "."
	}

	// The separator is kept when everything before it is a drive: the
	// directory of C:\adapter.log is C:\, not C:, and the difference decides
	// whether the refusal below names a volume root.
	if i == 2 && logFile[1] == ':' {
		return strings.ReplaceAll(logFile[:i+1], "/", `\`)
	}

	return strings.ReplaceAll(logFile[:i], "/", `\`)
}
