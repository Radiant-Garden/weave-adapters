package dhcpwindows

// AddressFamilyIPv4 is the only address family M3a serves. The field exists on
// Scope so IPv6 lands additively rather than as a breaking change: Windows
// models v6 scopes as a prefix rather than a start/end range, so folding both
// into one schema now would be a guess.
const AddressFamilyIPv4 = "ipv4"

// Scope is one Windows DHCP scope as the API serves it.
//
// The JSON tags mirror the PowerShell projection in listScopesScript exactly,
// field for field. That is the point of projecting explicitly rather than
// serializing cmdlet output: the wire contract is flat, camelCase, and stable
// across cmdlet property drift, and this struct can be read against the script
// side by side.
//
// Fields Windows exposes but this deliberately omits, because the
// no-speculative-code rule covers schema fields too:
//
//   - NapEnable, NapProfile — Network Access Protection was removed from
//     Windows Server in 2016. Surfacing them would put a dead Microsoft
//     technology into the contract weave consumes, permanently.
//   - CimClass, CimInstanceProperties, CimSystemProperties, PSComputerName —
//     transport and remoting metadata, not scope data.
//   - ActivatePolicies, Delay, MaxBootpClients — policy and failover tuning
//     with no consumer yet. Note if MaxBootpClients is ever added: it reads
//     4294967295 (uint32 max, meaning unlimited), which overflows int32 and
//     must not be projected with [int].
type Scope struct {
	// WadaptID is the derived, stable identity — the resource key, and the
	// pagination sort key. Never read from the backend; see identity.go.
	WadaptID string `json:"wadaptId"`

	// ScopeID is Windows' own key, the subnet address. Exposed as a filter
	// rather than as the resource key: private addresses repeat across
	// installations, so it is unique per server but not per fleet.
	ScopeID string `json:"scopeId"`

	SubnetMask string `json:"subnetMask"`
	StartRange string `json:"startRange"`
	EndRange   string `json:"endRange"`
	Name       string `json:"name"`

	// Description and SuperscopeName are omitempty because the convention is
	// that unsupported or unset fields are omitted rather than nulled, and
	// PowerShell hands back empty strings for both.
	Description string `json:"description,omitempty"`

	// State is Active or Inactive; Type is Dhcp, Bootp or Both. Both are cast
	// to strings in the projection rather than serialized as enums.
	State string `json:"state"`
	Type  string `json:"type"`

	// SuperscopeName is read-only. Surfacing the name a scope already carries
	// is not the same as modelling superscopes as a resource, which stays
	// deferred.
	SuperscopeName string `json:"superscopeName,omitempty"`

	// LeaseDurationSeconds is integer seconds per the API convention. Windows
	// carries a TimeSpan, which serializes as a {Ticks, Days, Hours, ...}
	// object unless projected — hence the explicit [int] cast.
	LeaseDurationSeconds int `json:"leaseDurationSeconds"`

	// AddressFamily is constant ipv4 in M3a. Set by the client rather than the
	// backend, since the v4 cmdlet does not report it.
	AddressFamily string `json:"addressFamily"`
}

// scriptPreamble opens every script, in this order. Neither line is optional
// and neither is version-specific.
//
// $ErrorActionPreference = 'Stop' is the more important of the two. The default
// is Continue, which means a permissions failure on Get-DhcpServerv4Scope
// writes to stderr, leaves the pipeline empty, exits *zero*, and serializes to
// an empty result. We would decode that as "this server has zero scopes" and
// return a cheerful 200 with an empty list — weave then sees a healthy adapter
// reporting no scopes and could reasonably conclude they were all deleted. Stop
// turns it into a terminating error and a non-zero exit.
//
// The encoding line closes a silent-corruption path. When powershell.exe 5.1's
// stdout is a pipe rather than a console it encodes using
// [Console]::OutputEncoding, which defaults to the OEM code page — so a scope
// named "Standort München" arrives as mojibake, and Go's encoding/json
// substitutes U+FFFD for invalid UTF-8 rather than erroring, decoding
// "successfully" into a corrupted name.
//
// New-Object System.Text.UTF8Encoding $false rather than
// [System.Text.Encoding]::UTF8: the latter carries a BOM preamble, and a BOM at
// the head of stdout would break the JSON decode.
const scriptPreamble = `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
`

// serverParams opens every script with the -ComputerName splat.
//
// Splatting an empty hashtable is how "no -ComputerName at all" is expressed, so
// the local-host case is the same code path as the remote one rather than a
// second script. Extracted because all three scripts need it verbatim, and a
// preamble that drifted in one of them would silently send that one command to
// the wrong host.
const serverParams = `$params = @{}
if ($env:` + envServerName + `) { $params['ComputerName'] = $env:` + envServerName + ` }
`

// scopeProjection is the Select-Object block that maps a Windows scope onto the
// Scope struct, field for field.
//
// It is one constant because it must be one shape. This block appeared three
// times — the listing, the create read-back, and once more besides — and
// createScopeScript's own doc comment claimed "a create and a read cannot
// disagree about shape" while only the listing copy was pinned by
// TestListScopesScript_ShouldProjectEveryModelField. A field added to one copy
// and missed in another decodes as a silently empty field on exactly one path,
// which is the failure class this package works hardest to close: no error, no
// log line, just a scope served with a blank name on the create response and a
// correct one on the read.
//
// The scripts are assembled by constant concatenation already, so sharing this
// costs nothing and makes the comment true by construction rather than by
// review. The leading newline lets a caller append it directly after a pipe.
//
// Properties, not methods: .IPAddressToString survives deserialization where
// .ToString() may not, and [string]$_.State works on both a live enum and a
// deserialized string.
const scopeProjection = `
  Select-Object @{n='scopeId';e={$_.ScopeId.IPAddressToString}},
                @{n='subnetMask';e={$_.SubnetMask.IPAddressToString}},
                @{n='startRange';e={$_.StartRange.IPAddressToString}},
                @{n='endRange';e={$_.EndRange.IPAddressToString}},
                @{n='name';e={$_.Name}},
                @{n='description';e={$_.Description}},
                @{n='state';e={[string]$_.State}},
                @{n='type';e={[string]$_.Type}},
                @{n='superscopeName';e={[string]$_.SuperscopeName}},
                @{n='leaseDurationSeconds';e={[int]$_.LeaseDuration.TotalSeconds}}
`

// listScopesScript reads every IPv4 scope as flat JSON.
//
// Every field is a calculated property even where the name would pass through
// unchanged: a bare `Name, Description` would emit Name/Description and break
// the camelCase convention on two fields while the other eight comply.
//
// Three PS 5.1 serialization behaviours make the explicit projection load
// bearing rather than tidy:
//
//   - -Depth defaults to 2, so nested values silently become the literal string
//     "System.Object[]". It is passed explicitly below.
//   - System.Net.IPAddress serializes as an object, not a string — you get
//     {"Address":…,"AddressFamily":…} where you wanted "10.0.0.0". Hence
//     .IPAddressToString on all four address fields.
//   - The cmdlet returns CimInstance objects, so every result carries CimClass,
//     CimInstanceProperties, CimSystemProperties and PSComputerName. A naive
//     ConvertTo-Json would serialize that plumbing into the payload.
//
// ConvertTo-Json -InputObject @(...) rather than a pipe: PS 5.1 unrolls a
// single-element result into a bare object rather than an array, so a server
// with exactly one scope would break the decoder. PS 5.1 has no -AsArray (that
// arrived in PS 6); wrapping in @() is the workaround, verified on the host.
//
// The projection itself is scopeProjection, shared with createScopeScript so
// the two cannot describe different shapes.
//
// Nothing is interpolated into this script — it is a constant, and the one
// value it needs arrives through the child process environment. That is the
// rule for every script in this package, here and once mutations land: set the
// value in exec.Cmd.Env, read $env:WADAPT_* in the script, splat it onto the
// cmdlet. No quoting, no injection surface, no temp file, and it works with
// -Command — which has no -ArgumentList, while -File would make execution
// policy relevant again.
const listScopesScript = scriptPreamble + serverParams +
	`$scopes = Get-DhcpServerv4Scope @params |` + scopeProjection +
	`ConvertTo-Json -InputObject @($scopes) -Depth 5
`

// probeScript is the health probe's single command: the cheapest query that
// still proves the whole path works, plus the shell's own identity.
//
// It reads scopes rather than checking the service, because
// `Get-Service DHCPServer` returning Running proves almost nothing we care
// about. The failure modes that actually bite are the DhcpServer module being
// absent — it ships with the RSAT-DHCP feature, which is *optional* even on a
// host running the DHCP Server role — and the service account lacking DHCP read
// rights. Both leave the service happily Running while every request fails. A
// real query collapses that gap: green means the module is present, permissions
// are right, and the server answers.
//
// The shell version and edition ride along because we are already running a
// script, so they cost nothing. When a host misbehaves in a way that turns out
// to be shell-version-dependent — PS 7's WinCompat shim returning deserialized
// objects with methods stripped, say — this is the field that ends the
// investigation in one request instead of a screen-share.
//
// Only each scope's id is projected, not the whole object: the probe wants to
// know the query works, not what it returned, and the ids keep the payload small
// while staying countable.
//
// The count is taken in Go rather than in PowerShell, deliberately. Whether
// `@($x).Count` is 0 or 1 when a pipeline produced nothing turns on whether the
// assignment left AutomationNull or $null in $x — a distinction two careful
// readings of the same shell disagreed about. Go has no such ambiguity: both
// `null` and `[]` unmarshal to a slice of length zero. A freshly provisioned
// server with no scopes yet is the case that would otherwise report one scope
// that does not exist, to an operator checking whether provisioning worked.
const probeScript = scriptPreamble + serverParams +
	`$scopeIds = Get-DhcpServerv4Scope @params |
  Select-Object @{n='scopeId';e={$_.ScopeId.IPAddressToString}}
ConvertTo-Json -InputObject @{
  scopes     = @($scopeIds)
  psVersion  = [string]$PSVersionTable.PSVersion
  psEdition  = [string]$PSVersionTable.PSEdition
} -Depth 5
`

// Environment variables carrying createScopeScript's parameters.
//
// Every value a create needs travels this way. Nothing is interpolated into the
// script text — see the runner interface for why that rule exists, and why the
// obvious alternatives (-ArgumentList, a param() block) are not available with
// -Command.
const (
	envScopeID          = "WADAPT_SCOPE_ID"
	envScopeName        = "WADAPT_SCOPE_NAME"
	envScopeStartRange  = "WADAPT_SCOPE_START"
	envScopeEndRange    = "WADAPT_SCOPE_END"
	envScopeSubnetMask  = "WADAPT_SCOPE_MASK"
	envScopeDescription = "WADAPT_SCOPE_DESCRIPTION"
	envScopeLease       = "WADAPT_SCOPE_LEASE"
	envScopeState       = "WADAPT_SCOPE_STATE"
	envScopeType        = "WADAPT_SCOPE_TYPE"
)

// conflictMarker is what createScopeScript prints when the subnet already holds
// a scope.
//
// A marker rather than the shell's own error text, because that text is
// localized — the WS2022 host this was developed against answers in German, so
// matching English would pass in review and fail in production. The script
// decides while it still has structured data; Go reads a stable ASCII token.
const conflictMarker = "WADAPT_CONFLICT"

// createScopeScript adds one scope and emits it through scopeProjection — the
// same constant listScopesScript uses, so a create and a read cannot disagree
// about shape. That was a claim this comment made while the block was copied
// into both scripts and only the listing copy was pinned by a test; sharing the
// constant is what makes it structural.
//
// The subnet is checked BEFORE the create rather than classified afterwards.
// Windows permits one scope per subnet, and that is the one failure a client
// can act on, so it earns a clean answer instead of an error message parsed out
// of a localized exception. Everything else keeps its own message and becomes a
// backend error, because inventing categories for failures nobody has observed
// is how a taxonomy fills with entries that never match.
//
// ⚠️ That check is not atomic. Two creates racing on one subnet can both pass
// it, and the loser then fails inside Add-DhcpServerv4Scope and surfaces as a
// generic backend error rather than a 409. Accepted rather than hidden: the
// scope is still not created twice, Windows guarantees that, and the honest
// alternative — parsing localized exception text — trades a rare wrong status
// code for a common one.
//
// scopeId arrives computed rather than derived here: Go needs it anyway to
// build the wadaptID, and computing it twice in two languages is how the two
// come to disagree.
//
// -PassThru returns the created scope, so there is no re-read to race with
// another client's edit. Parameters are typed before use — the cmdlet wants
// [IPAddress] and [TimeSpan], and the coercion PowerShell applies to a bare
// string is not always the one intended.
//
// Optional parameters are added only when their variable is non-empty, which is
// what makes "omitted" mean "Windows applies its own default" rather than "the
// adapter chose one for you". A scope created without a lease duration gets the
// server's default, and that default stays the server's to change.
const createScopeScript = scriptPreamble + serverParams + `
$existing = @(Get-DhcpServerv4Scope @params |
  Where-Object { $_.ScopeId.IPAddressToString -eq $env:` + envScopeID + ` })
if ($existing.Count -gt 0) {
  Write-Output '` + conflictMarker + `'
  exit 0
}

$create = $params.Clone()
$create['Name']       = $env:` + envScopeName + `
$create['StartRange'] = [System.Net.IPAddress]::Parse($env:` + envScopeStartRange + `)
$create['EndRange']   = [System.Net.IPAddress]::Parse($env:` + envScopeEndRange + `)
$create['SubnetMask'] = [System.Net.IPAddress]::Parse($env:` + envScopeSubnetMask + `)

if ($env:` + envScopeDescription + `) { $create['Description']   = $env:` + envScopeDescription + ` }
if ($env:` + envScopeState + `)       { $create['State']         = $env:` + envScopeState + ` }
if ($env:` + envScopeType + `)        { $create['Type']          = $env:` + envScopeType + ` }
if ($env:` + envScopeLease + `)       { $create['LeaseDuration'] = [TimeSpan]::FromSeconds([int]$env:` + envScopeLease + `) }

$scope = Add-DhcpServerv4Scope @create -PassThru |` + scopeProjection +
	`ConvertTo-Json -InputObject @($scope) -Depth 5
`
