# Grants an account read-only DHCP rights on this host, so the e2e gate can
# observe the adapter serving real scopes.
#
# RUN THIS ONCE, IN AN ELEVATED POWERSHELL, ON THE DHCP HOST.
#
# It cannot run from CI, and that is not an oversight: creating accounts and
# editing security groups needs Administrator, and a CI job that could grant
# itself DHCP rights would be a privilege-escalation path rather than a
# convenience. The runner executes as NETWORK SERVICE precisely so it cannot.
#
# KEEP THIS FILE ASCII-ONLY. Windows PowerShell 5.1 decodes a BOM-less .ps1
# using the host's ANSI codepage; on a German-locale host that is CP1252, where
# the third byte of a UTF-8 em dash lands on U+201D -- which PowerShell honours
# as a closing double quote. One em dash inside a string silently ends it, and
# the parse error names a column rather than the character.

[CmdletBinding()]
param(
    # The principal to grant. Defaults to the SID of NETWORK SERVICE, which is
    # what a default GitHub Actions runner executes as.
    #
    # A SID rather than a name, deliberately: account names are localized, so
    # "NT AUTHORITY\NetworkService" does not resolve on a German host, where the
    # same account is "NT-AUTORITAET\Netzwerkdienst". SIDs are invariant.
    #
    # PREFER A DEDICATED ACCOUNT. Granting NETWORK SERVICE means every service
    # on this host running under that identity inherits DHCP read access -- you
    # are granting a class, not a service. Pass -Account 'svc-weave-adapter' (or
    # whatever you named it) and run the runner service as that instead.
    [string]$Account = 'S-1-5-20',

    # The read-only DHCP group. Left empty, the script finds it, because the name
    # is localized too ("DHCP-Benutzer" on a German host).
    [string]$Group = '',

    # The GitHub Actions runner service. Group membership is stamped into an
    # access token at logon, so a running service keeps its old token and goes on
    # failing with WIN32 5 until it restarts.
    [string]$RunnerService = 'actions.runner.*',

    [switch]$SkipRunnerRestart
)

$ErrorActionPreference = 'Stop'

function Write-Step { param([string]$Text) Write-Host "==> $Text" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Text) Write-Host "    OK: $Text" -ForegroundColor Green }
function Write-Warn { param([string]$Text) Write-Host "    !!  $Text" -ForegroundColor Yellow }

# --- elevation -------------------------------------------------------------

$identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)

if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "This script edits a security group, which needs Administrator." -ForegroundColor Red
    Write-Host "Re-run it from an elevated PowerShell (Run as administrator)."   -ForegroundColor Red
    exit 1
}

Write-Step "Host"
Write-Host "    computer : $env:COMPUTERNAME"
Write-Host "    running  : $($identity.Name)"

# --- domain role -----------------------------------------------------------
#
# Local groups do not exist on a domain controller, and NETWORK SERVICE is a
# local principal that cannot be added to an AD group at all -- on a DC it
# authenticates to other services as the computer account. Detect it and say so
# rather than failing with a confusing error twenty lines later.

$role = (Get-CimInstance Win32_ComputerSystem).DomainRole
$isDomainController = ($role -eq 4 -or $role -eq 5)

Write-Host "    role     : $role $(if ($isDomainController) { '(DOMAIN CONTROLLER)' } else { '(member/standalone server)' })"

if ($isDomainController) {
    Write-Host ""
    Write-Host "This host is a domain controller, so this script cannot finish the job." -ForegroundColor Red
    Write-Host ""
    Write-Host "Local groups do not exist here, and NETWORK SERVICE is a local principal"
    Write-Host "that cannot be added to an AD group -- on a DC it authenticates to other"
    Write-Host "services as the computer account. Two real options:"
    Write-Host ""
    Write-Host "  1. Grant the computer account:"
    Write-Host "       Add-ADGroupMember -Identity 'DHCP Users' -Members '$env:COMPUTERNAME`$'"
    Write-Host ""
    Write-Host "  2. Better: run the runner service as a dedicated domain account and"
    Write-Host "     grant only that:"
    Write-Host "       Add-ADGroupMember -Identity 'DHCP Users' -Members 'svc-weave-adapter'"
    Write-Host ""
    Write-Host "Option 2 is the one that matches the milestone's exit criterion -- a"
    Write-Host "read-only service account serving every endpoint."
    exit 1
}

# --- find the group --------------------------------------------------------

if ([string]::IsNullOrWhiteSpace($Group)) {
    Write-Step "Finding the read-only DHCP group (the name is localized)"

    $candidates = @(Get-LocalGroup | Where-Object {
        $_.Name -match 'DHCP' -and $_.Name -notmatch 'Admin'
    })

    if ($candidates.Count -eq 0) {
        Write-Host "No DHCP group found on this host." -ForegroundColor Red
        Write-Host "The DHCP Server role creates 'DHCP Users' and 'DHCP Administrators' when it"
        Write-Host "is installed. Groups present that mention DHCP:"
        Get-LocalGroup | Where-Object Name -match 'DHCP' | Format-Table Name, Description -AutoSize
        Write-Host "Pass the right one explicitly:  -Group '<name>'"
        exit 1
    }

    if ($candidates.Count -gt 1) {
        Write-Host "More than one candidate group; pass one explicitly with -Group:" -ForegroundColor Red
        $candidates | Format-Table Name, Description -AutoSize
        exit 1
    }

    $Group = $candidates[0].Name
}

Write-Ok "group: $Group"

# --- warn on the over-grant ------------------------------------------------

if ($Account -eq 'S-1-5-20') {
    Write-Warn "Granting NETWORK SERVICE (S-1-5-20)."
    Write-Warn "That identity is shared by many Windows services, so every one of them on"
    Write-Warn "this host inherits DHCP read access. It works, but it grants a class rather"
    Write-Warn "than a service. A dedicated account is the better shape:"
    Write-Warn "  New-LocalUser -Name svc-weave-adapter -Description 'weave-adapter read-only DHCP'"
    Write-Warn "  .\grant-dhcp-access.ps1 -Account svc-weave-adapter"
    Write-Warn "  ...then run the runner service as that account."
}

# --- grant -----------------------------------------------------------------

Write-Step "Granting $Account read access via '$Group'"

$existing = @(Get-LocalGroupMember -Group $Group -ErrorAction SilentlyContinue)
$already  = $existing | Where-Object { $_.SID.Value -eq $Account -or $_.Name -like "*$Account" }

if ($already) {
    Write-Ok "already a member, nothing to do"
} else {
    Add-LocalGroupMember -Group $Group -Member $Account
    Write-Ok "added"
}

Write-Step "Members of '$Group'"
Get-LocalGroupMember -Group $Group | Format-Table Name, PrincipalSource -AutoSize

# --- restart the runner ----------------------------------------------------
#
# The load-bearing step. Group membership lands in an access token at logon, so
# the already-running service keeps the token it started with and goes on being
# denied. Skipping this is the single likeliest way to conclude the grant did
# not work.

if ($SkipRunnerRestart) {
    Write-Warn "Skipping the runner restart, as asked."
    Write-Warn "The new membership does NOT apply to the running service until it restarts:"
    Write-Warn "  Get-Service $RunnerService | Restart-Service"
    exit 0
}

Write-Step "Restarting the runner so it picks up the new membership"

$runners = @(Get-Service $RunnerService -ErrorAction SilentlyContinue)

if ($runners.Count -eq 0) {
    Write-Warn "No service matching '$RunnerService' on this host."
    Write-Warn "If the adapter or the runner runs under a different service name, restart it"
    Write-Warn "yourself -- the grant does not reach a process that is already running."
} else {
    $runners | Restart-Service
    Start-Sleep -Seconds 2
    $runners | Get-Service | Format-Table Name, Status, StartType -AutoSize
    Write-Ok "restarted"
}

Write-Host ""
Write-Host "Done. Re-run the Windows verify workflow; the e2e gate should now find the" -ForegroundColor Green
Write-Host "dhcp-server component healthy instead of PermissionDenied (WIN32 5)."        -ForegroundColor Green
