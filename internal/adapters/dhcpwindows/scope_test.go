/*
Testing: scope.go

Pending:

	Running the projection against a real WS2022 host and diffing its output
	  against testdata/scopes_single.json. The fixture *is* a host capture, but
	  nothing re-verifies it automatically, so a projection edit that breaks on
	  the host would pass here. That check belongs with the M1/M3a host sign-off.

Tested:

	scriptPreamble
	  - TestScriptPreamble_ShouldOpenEveryScriptWithBothGuards: the two lines, in
	    order, on every script. Neither is optional.
	  - TestScriptPreamble_ShouldRequestBOMLessUTF8: [System.Text.Encoding]::UTF8
	    would put a BOM at the head of stdout and break the decode.

	listScopesScript
	  - TestListScopesScript_ShouldProjectEveryModelField: the projection and the
	    struct tags name the same ten fields — the pair that silently drifts.
	  - TestListScopesScript_ShouldWrapTheResultInAnArray: the PS 5.1 one-element trap.
	  - TestListScopesScript_ShouldPassDepthExplicitly: -Depth defaults to 2.
	  - TestListScopesScript_ShouldReadTheServerFromTheEnvironment: no interpolation.
	  - TestListScopesScript_ShouldAvoidConstructsPS51CannotParse: the 5.1/7
	    compatibility rules, which are invisible until a host fails.

	Scope
	  - TestScope_ShouldOmitEmptyOptionalFieldsOnly: description and superscopeName
	    are omitempty; the rest are always present.

	deleteScopeScript / updateScopeScript
	  - TestUpdateScopeScript_ShouldProjectEveryModelField: the update read-back
	    projects the same fields the list does — the pair that silently drifts.
	  - TestMutationScripts_ShouldReadValuesFromTheEnvironment: no value is
	    interpolated into either write script.
	  - TestUpdateScopeScript_ShouldSplatRangeAsAPair: -StartRange and -EndRange
	    share one guard, because the WithRange set is mandatory-both.
	  Both scripts are also covered by TestScriptPreamble_ShouldOpenEveryScriptWithBothGuards
	  and TestListScopesScript_ShouldAvoidConstructsPS51CannotParse via the script maps.

Tested elsewhere:

	Decoding real captured output into the model: client_test.go.

Declined:

	Executing the scripts: they need powershell.exe and a DHCP server, so they
	  cannot run in CI on darwin. The scripts are asserted as text here and
	  verified against the host out of band — which is why the assertions are
	  about the specific traps rather than about the script reading nicely.
	probeScript's decoded field shape: covered where it is consumed, in
	  probe_test.go, rather than asserted twice.

Additional Remarks:

	These are text assertions on a constant, which is unusual and deliberate. Each
	  one corresponds to a specific documented PS 5.1 serialization trap that
	  produces silent corruption rather than an error — so the cost of an
	  accidental edit is a wrong answer served confidently, and a text assertion is
	  the only guard available without a Windows host in CI.
*/
package dhcpwindows

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScriptPreamble_ShouldOpenEveryScriptWithBothGuards(t *testing.T) {
	t.Parallel()

	scripts := map[string]string{
		"listScopesScript":  listScopesScript,
		"probeScript":       probeScript,
		"createScopeScript": createScopeScript,
		"deleteScopeScript": deleteScopeScript,
		"updateScopeScript": updateScopeScript,
	}

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT
			lines := strings.Split(script, "\n")
			require.GreaterOrEqual(t, len(lines), 2)

			// ASSERT — order matters: the error preference has to be set before
			// anything can fail. Stop is what stops a permissions failure
			// exiting zero with an empty pipeline, which we would otherwise
			// serve as "this server has zero scopes".
			assert.Equal(t, `$ErrorActionPreference = 'Stop'`, lines[0])
			assert.Equal(t, `[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false`, lines[1])
		})
	}
}

func TestScriptPreamble_ShouldRequestBOMLessUTF8(t *testing.T) {
	t.Parallel()

	// ASSERT — New-Object System.Text.UTF8Encoding $false, never
	// [System.Text.Encoding]::UTF8: the latter carries a BOM preamble, and a
	// BOM landing at the head of stdout breaks the JSON decode.
	assert.Contains(t, scriptPreamble, "New-Object System.Text.UTF8Encoding $false")
	assert.NotContains(t, scriptPreamble, "[System.Text.Encoding]::UTF8")
}

func TestListScopesScript_ShouldProjectEveryModelField(t *testing.T) {
	t.Parallel()

	// ARRANGE — the JSON names the model expects from the backend. WadaptID and
	// AddressFamily are excluded: both are set by the client, not projected.
	clientSet := map[string]bool{"wadaptId": true, "addressFamily": true}

	// ACT / ASSERT — every backend-sourced field must appear in the projection
	// as a calculated property. This is the assertion that catches the script
	// and the struct tags drifting apart, which would surface as a silently
	// empty field rather than an error.
	for field := range reflect.TypeFor[Scope]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if clientSet[name] {
			continue
		}

		assert.Contains(t, listScopesScript, "n='"+name+"'",
			"Scope.%s is not projected by listScopesScript", field.Name)
	}
}

func TestListScopesScript_ShouldWrapTheResultInAnArray(t *testing.T) {
	t.Parallel()

	// ASSERT — PS 5.1 unrolls a single-element result into a bare object, and
	// has no -AsArray (that arrived in PS 6). @(...) is the workaround,
	// verified on the host.
	assert.Contains(t, listScopesScript, "ConvertTo-Json -InputObject @($scopes)")
	assert.NotContains(t, listScopesScript, "-AsArray")
}

func TestListScopesScript_ShouldPassDepthExplicitly(t *testing.T) {
	t.Parallel()

	// ASSERT — -Depth defaults to 2, at which nested values silently become the
	// literal string "System.Object[]".
	assert.Contains(t, listScopesScript, "-Depth")
	assert.Contains(t, probeScript, "-Depth")
}

func TestListScopesScript_ShouldReadTheServerFromTheEnvironment(t *testing.T) {
	t.Parallel()

	// ASSERT — the one value the script needs arrives through the child
	// process environment and is splatted onto the cmdlet. No quoting, no
	// injection surface, and it works with -Command, which has no
	// -ArgumentList.
	assert.Contains(t, listScopesScript, "$env:"+envServerName)
	assert.Contains(t, listScopesScript, "$params['ComputerName']")
	assert.Contains(t, listScopesScript, "Get-DhcpServerv4Scope @params")

	// A format verb would mean something builds this script at runtime.
	assert.NotContains(t, listScopesScript, "%s")
}

func TestListScopesScript_ShouldAvoidConstructsPS51CannotParse(t *testing.T) {
	t.Parallel()

	// ARRANGE — PS 5.1 is the target and PS 7 is not a prerequisite, so no
	// script may use a construct that only parses in 7. These fail at parse
	// time on a 5.1 host, i.e. on the customer's server rather than here.
	banned := []string{
		"-AsArray",           // PS 6+
		"??",                 // null-coalescing, PS 7+
		"-Parallel",          // ForEach-Object -Parallel, PS 7+
		"$PSStyle",           // PS 7.2+
		"ConvertFrom-Json -", // no switches we rely on differ, but flag drift
	}

	scripts := map[string]string{
		"listScopesScript":  listScopesScript,
		"probeScript":       probeScript,
		"createScopeScript": createScopeScript,
		"deleteScopeScript": deleteScopeScript,
		"updateScopeScript": updateScopeScript,
	}

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ASSERT
			for _, construct := range banned {
				assert.NotContains(t, script, construct)
			}

			// Properties, not methods: .IPAddressToString survives
			// deserialization where .ToString() may not, which matters if a
			// host ever loads the module through PS 7's WinCompat shim.
			assert.NotContains(t, script, ".ToString()")
		})
	}
}

func TestScope_ShouldOmitEmptyOptionalFieldsOnly(t *testing.T) {
	t.Parallel()

	// ARRANGE — a scope with both optional fields empty, which is what the host
	// returns for a scope with no description and no superscope.
	scope := Scope{
		WadaptID:      "au0r6itmalpoo",
		ScopeID:       "192.168.178.0",
		AddressFamily: AddressFamilyIPv4,
	}

	// ACT
	raw, err := json.Marshal(scope)
	require.NoError(t, err)

	body := string(raw)

	// ASSERT — the convention is that unsupported or unset fields are omitted
	// rather than nulled, and PowerShell hands back empty strings for both.
	assert.NotContains(t, body, "description")
	assert.NotContains(t, body, "superscopeName")

	// Everything else is always present, including the zero lease duration:
	// omitting a numeric field would make "unset" and "zero" indistinguishable
	// to a consumer.
	assert.Contains(t, body, `"leaseDurationSeconds":0`)
	assert.Contains(t, body, `"state":""`)
	assert.Contains(t, body, `"addressFamily":"ipv4"`)
}

func TestUpdateScopeScript_ShouldProjectEveryModelField(t *testing.T) {
	t.Parallel()

	// ARRANGE — the update reads the changed scope back through the shared
	// projection, so every backend-sourced model field must appear. WadaptID and
	// AddressFamily are client-set, not projected.
	clientSet := map[string]bool{"wadaptId": true, "addressFamily": true}

	// ACT / ASSERT — the same drift-catcher the list has: a field added to the
	// struct but missed in the projection would decode as silently empty on the
	// update response.
	for field := range reflect.TypeFor[Scope]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if clientSet[name] {
			continue
		}

		assert.Contains(t, updateScopeScript, "n='"+name+"'",
			"Scope.%s is not projected by updateScopeScript", field.Name)
	}
}

func TestMutationScripts_ShouldReadValuesFromTheEnvironment(t *testing.T) {
	t.Parallel()

	scripts := map[string]string{
		"deleteScopeScript": deleteScopeScript,
		"updateScopeScript": updateScopeScript,
	}

	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ASSERT — the write scripts take every value through the child
			// environment, never interpolated into the command text. A format verb
			// would mean something builds the script at runtime, which is the
			// injection surface the whole env-passing design exists to keep at zero.
			assert.Contains(t, script, "$env:"+envScopeID)
			assert.NotContains(t, script, "%s")
			assert.NotContains(t, script, "10.0.", "no scope value may be baked into the script")
		})
	}
}

func TestUpdateScopeScript_ShouldSplatRangeAsAPair(t *testing.T) {
	t.Parallel()

	// ASSERT — -StartRange and -EndRange are a mandatory-together parameter set,
	// so both are set under one guard. A guard on the end alone would let a
	// one-sided splat bind the WithRange set with a missing parameter and fail.
	assert.Contains(t, updateScopeScript, "$set['StartRange']")
	assert.Contains(t, updateScopeScript, "$set['EndRange']")
	assert.Contains(t, updateScopeScript, "if ($env:"+envScopeStartRange+")")
	assert.NotContains(t, updateScopeScript, "if ($env:"+envScopeEndRange+")",
		"the end must ride inside the start's guard, not have its own")
}
