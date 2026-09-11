package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ServiceWorkingDirectory is where the Windows Service Control Manager starts
// a service. It appears in the error messages below because it is the whole
// reason they exist, and because an operator who has not met this before will
// not guess it.
const ServiceWorkingDirectory = `C:\Windows\System32`

// CheckServicePaths reports every configured path that a service could not
// use: one that would resolve somewhere else, and one that is not set at all
// when a service cannot do without it.
//
// Under the SCM the working directory is C:\Windows\System32 rather than
// wherever the operator installed the adapter, so a relative path silently
// points at a directory nobody intended. The symptom is a service that fails
// at startup with a file-not-found naming a path the operator can see exists
// — which reads as a bug in the adapter rather than as a configuration
// mistake, and is why this is checked rather than documented.
//
// Every failure is reported at once, and each names the key an operator
// actually sets: fixing one relative path only to hit the next one on the
// following boot is the version of this that wastes an afternoon.
func CheckServicePaths(v *Values) error {
	var errs []error

	// Sorted so the report is stable, and so an operator comparing two runs is
	// comparing the same order.
	names := make([]string, 0, len(v.spec))
	for name := range v.spec {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		key := v.spec[name]
		if key.Type != TypeString {
			continue
		}

		value := v.String(name)

		if key.ServiceRequired && value == "" {
			errs = append(errs, fmt.Errorf(
				"%s is not set, and a service needs it: the Service Control Manager discards stdout, "+
					"so the adapter would run correctly and log nowhere. Set an absolute path (%s, or the "+
					"%s key in the config file)",
				key.Name, FlagName(key.Name), key.Name,
			))

			continue
		}

		if key.Path == NotAPath {
			continue
		}

		if err := checkPath(key, value); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// checkPath reports whether one value is usable from a service.
func checkPath(key Key, value string) error {
	// An empty value is not a path yet. Whether that is allowed is the
	// owning package's rule — core rejects an empty authTokensFile when auth
	// is on, and accepts an empty logFile as "write to stdout" — and second-
	// guessing it here would reject a default that is deliberately unset.
	if value == "" {
		return nil
	}

	if isAbsoluteWindowsPath(value) {
		return nil
	}

	if key.Path == CommandPath && isBareCommandName(value) {
		return nil
	}

	return fmt.Errorf(
		"%s is %q, which is relative: a service resolves it against %s, not against the "+
			"directory you installed into. Set an absolute path (%s, or the %s key in the config file)",
		key.Name, value, ServiceWorkingDirectory, FlagName(key.Name), key.Name,
	)
}

// isBareCommandName reports whether value is a command to be resolved through
// PATH rather than a path relative to the working directory.
//
// The distinction matters because Go's exec.LookPath does not search the
// working directory on Windows, so "powershell.exe" cannot resolve against
// C:\Windows\System32 by accident the way ".\powershell.exe" would. A bare
// name is therefore as safe under a service as it is from a console, while
// anything carrying a separator is exactly the trap.
func isBareCommandName(value string) bool {
	return !strings.ContainsAny(value, `/\`)
}

// isAbsoluteWindowsPath reports whether value is absolute under WINDOWS rules,
// whatever host this is running on.
//
// filepath.IsAbs is deliberately not used. It applies the host's rules, so on
// a developer machine `C:\ProgramData\wadapt\tokens.toml` reads as relative
// and `/etc/wadapt/tokens.toml` reads as absolute — both backwards for the
// only platform this check is about. Encoding the rule here instead makes it
// correct where it runs and testable where it does not.
//
// Drive-relative forms are rejected on purpose. `\wadapt\tokens.toml` resolves
// against the current drive and `C:tokens.toml` against that drive's current
// directory, and under the SCM both land somewhere derived from
// C:\Windows\System32 — which is the trap wearing a different hat.
func isAbsoluteWindowsPath(value string) bool {
	norm := strings.ReplaceAll(value, "/", `\`)

	// UNC: two leading separators, then a server name.
	if strings.HasPrefix(norm, `\\`) {
		return true
	}

	// Drive-absolute: a single letter, a colon, then a separator.
	if len(norm) >= 3 && norm[1] == ':' && norm[2] == '\\' && isDriveLetter(norm[0]) {
		return true
	}

	return false
}

// isDriveLetter reports whether c names a drive.
func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
