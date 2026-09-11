package winsvc

import (
	"errors"
	"fmt"
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

// ErrNotSecured reports that a target's permissions are wider than the policy
// allows.
var ErrNotSecured = errors.New("winsvc: the path grants access beyond SYSTEM and Administrators")

// Grant is one access-control entry read back from a target.
type Grant struct {
	// SID is the trustee, as a string.
	SID string
	// CanWrite reports whether the entry confers any kind of write.
	CanWrite bool
}

// CheckGrants reports whether the entries read from a target stay within the
// policy.
//
// Only writes are refused. A read of the token store leaks hashes, which
// cannot be replayed, and refusing every stray read entry would make the check
// fail on hosts with a benign auditing or backup grant — a rule nobody could
// satisfy is a rule that gets disabled. A write is the actual escalation, on
// all three targets.
func CheckGrants(path string, grants []Grant) error {
	allowed := LockdownGrantees()

	var offenders []string

	for _, g := range grants {
		if !g.CanWrite {
			continue
		}

		if !containsSID(allowed, g.SID) {
			offenders = append(offenders, g.SID)
		}
	}

	if len(offenders) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s grants write to %v; run `weave-adapter-dhcp-windows service secure` "+
		"from an elevated prompt", ErrNotSecured, path, offenders)
}

// containsSID reports whether sid is in the list, case-insensitively: Windows
// renders a SID string in upper case but accepts either.
func containsSID(list []string, sid string) bool {
	return slices.ContainsFunc(list, func(s string) bool {
		return strings.EqualFold(s, sid)
	})
}

// logDirOf returns the directory a log file lives in.
func logDirOf(logFile string) string {
	return filepath.Dir(logFile)
}
