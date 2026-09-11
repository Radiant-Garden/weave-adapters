/*
Testing: servicepath.go

Pending:

Tested:

	isAbsoluteWindowsPath -> - covered through CheckServicePaths' accept and
	                           reject tables, which is where the distinction is
	                           operator-visible.
	CheckServicePaths -> - TestCheckServicePaths_ShouldAcceptWhatAServiceCanResolve
	                     - TestCheckServicePaths_ShouldRejectARelativePath
	                     - TestCheckServicePaths_ShouldReportEveryOffendingKeyAtOnce
	                     - TestCheckServicePaths_ShouldNameTheKeyAndTheWorkingDirectory
	                     - TestCheckServicePaths_ShouldIgnoreKeysThatAreNotPaths
	isBareCommandName -> - TestIsBareCommandName_ShouldAcceptOnlyASeparatorFreeName

Tested elsewhere:

	That the installer and the service startup path both call this, and that
	the installer resolves without the environment:
	cmd/weave-adapter-dhcp-windows/service_test.go and main_test.go.

	Which keys are classified as paths: each key's own registration -- CoreKeys
	here, and dhcpwindows.Keys for the adapter.

Declined:

	Testing against a real C:\Windows\System32. The rule is about whether a
	path is absolute, not about what any particular directory contains, and a
	test that needed the directory to exist would only run on Windows -- for a
	check whose whole value is that it runs everywhere.

Additional Remarks:

	The absoluteness rule is WINDOWS' rule, applied on every host, and these
	tests are why that had to be so. filepath.IsAbs uses the host's rules, so
	the first version read C:\ProgramData\... as relative on macOS and
	/etc/... as absolute -- backwards for the only platform the check is about,
	and untestable off Windows besides.

	An empty value is deliberately accepted. Whether empty is allowed is the
	owning package's rule -- core rejects an empty authTokensFile when auth is
	on, and reads an empty logFile as "write to stdout" -- and second-guessing
	it here would reject a default that is deliberately unset.
*/
package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathSpec is a spec with one key of each classification, so a case can set
// exactly the value it is about.
func pathSpec() Spec {
	return Spec{
		{Name: "tokenStore", Type: TypeString, Path: FilePath, Default: ""},
		{Name: "shell", Type: TypeString, Path: CommandPath, Default: ""},
		{Name: "serverName", Type: TypeString, Default: ""},
		{Name: "port", Type: TypeInt, Default: 8444},
	}
}

// loadPathSpec loads pathSpec with the given flags and no environment.
func loadPathSpec(t *testing.T, args ...string) *Values {
	t.Helper()

	v, err := LoadWithoutEnvironment(pathSpec(), args)
	require.NoError(t, err)

	return v
}

func TestCheckServicePaths_ShouldAcceptWhatAServiceCanResolve(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"should accept a drive-absolute path":      {"--token-store", `C:\ProgramData\wadapt\tokens.toml`},
		"should accept a forward-slash drive path": {"--token-store", "C:/ProgramData/wadapt/tokens.toml"},
		"should accept a UNC path":                 {"--token-store", `\\fileserver\wadapt\tokens.toml`},
		"should accept a bare command name":        {"--shell", "powershell.exe"},
		"should accept an absolute command":        {"--shell", `C:\Windows\System32\pwsh.exe`},
		"should accept everything unset":           {},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.NoError(t, CheckServicePaths(loadPathSpec(t, args...)))
		})
	}
}

func TestCheckServicePaths_ShouldRejectARelativePath(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"should reject a bare filename":        {"--token-store", "tokens.toml"},
		"should reject a dot-relative path":    {"--token-store", "./tokens.toml"},
		"should reject a subdirectory":         {"--token-store", `data\tokens.toml`},
		"should reject a parent-relative path": {"--token-store", "../tokens.toml"},
		// Drive-relative, not absolute: it resolves against the current drive
		// or that drive's current directory, which under the SCM is derived
		// from C:\Windows\System32 -- the same trap wearing a different hat.
		"should reject a drive-relative rooted path": {"--token-store", `\wadapt\tokens.toml`},
		"should reject a drive-relative path":        {"--token-store", "C:tokens.toml"},
		// A unix path is not a Windows path. This check exists for one
		// platform, and a POSIX absolute path is not absolute there.
		"should reject a unix absolute path": {"--token-store", "/etc/wadapt/tokens.toml"},
		// A command WITH a separator is a path, and it is relative. The bare-name
		// exemption is exactly and only about names PATH resolves.
		"should reject a relative command path": {"--shell", `bin\pwsh.exe`},
	}

	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT
			err := CheckServicePaths(loadPathSpec(t, args...))

			// ASSERT
			require.Error(t, err)
		})
	}
}

func TestCheckServicePaths_ShouldReportEveryOffendingKeyAtOnce(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckServicePaths(loadPathSpec(t,
		"--token-store", "tokens.toml",
		"--shell", `bin\pwsh.exe`,
	))

	// ASSERT
	// Joined, for the same reason Validate joins: fixing one relative path
	// only to hit the next on the following boot is the version of this that
	// costs an afternoon.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenStore")
	assert.Contains(t, err.Error(), "shell")
}

func TestCheckServicePaths_ShouldNameTheKeyAndTheWorkingDirectory(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	err := CheckServicePaths(loadPathSpec(t, "--token-store", "tokens.toml"))

	// ASSERT
	// The message is the feature. An operator who has not met this before will
	// not guess that a service runs from System32, and the symptom -- a
	// file-not-found naming a path they can see exists -- reads as a bug in
	// the adapter.
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "tokenStore")
	assert.Contains(t, msg, "tokens.toml")
	assert.Contains(t, msg, ServiceWorkingDirectory)
	assert.Contains(t, msg, "token-store", "the message should name the flag an operator sets")
}

func TestCheckServicePaths_ShouldIgnoreKeysThatAreNotPaths(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	// A server name that looks path-ish, and an int. Neither is classified as
	// a path, so neither is this check's business.
	err := CheckServicePaths(loadPathSpec(t, "--server-name", "some/relative/looking/value", "--port", "9000"))

	// ASSERT
	assert.NoError(t, err)
}

func TestIsBareCommandName_ShouldAcceptOnlyASeparatorFreeName(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"powershell.exe":                     true,
		"pwsh":                               true,
		`bin\pwsh.exe`:                       false,
		"bin/pwsh":                           false,
		`C:\Windows\System32\powershell.exe`: false,
		"./pwsh":                             false,
	}

	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			// ARRANGE / ACT / ASSERT
			assert.Equal(t, want, isBareCommandName(value))
		})
	}
}
