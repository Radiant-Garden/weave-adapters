# Grants an account the rights the adapter needs to read Windows DHCP, so the
# e2e gate can observe it serving real scopes.
#
# RUN THIS ONCE, IN AN ELEVATED POWERSHELL, ON THE DHCP HOST.
#
# It cannot run from CI, and that is not an oversight: editing a security group
# needs Administrator, and a CI job able to grant itself these rights would be a
# privilege-escalation path rather than a convenience.
#
# ---------------------------------------------------------------------------
# WHAT THIS GRANTS, AND WHY IT IS MORE THAN YOU WOULD EXPECT
# ---------------------------------------------------------------------------
#
# It adds the account to the BUILT-IN ADMINISTRATORS group. That is a large
# grant and the script says so loudly, but it is the only thing that works.
#
# Windows has a group that looks exactly right -- "DHCP Users", described as
# "members who have read-only access to the DHCP service". It does not help
# here. Measured on WS2022 (2026-07-20, host WIN-01):
#
#   NETWORK SERVICE in DHCP Users .............. denied (WIN32 5)
#   NETWORK SERVICE in DHCP Administrators ..... denied (WIN32 5)
#   ordinary local user in DHCP Users .......... denied (WIN32 5)
#   local administrator ........................ works
#
# The DHCP groups govern the DHCP console and netsh. Get-DhcpServerv4Scope goes
# through WMI (root\Microsoft\Windows\DHCP), which gates on Administrators, so
# neither DHCP group reaches it. Granting them is not a smaller version of this
# grant -- it does nothing at all, which is worse, because it looks like it did
# something.
#
# CONSEQUENCES WORTH KNOWING BEFORE YOU RUN THIS:
#
#   - The adapter requires local administrator on the DHCP host to read scopes
#     through the PowerShell transport. That is a property of the transport, not
#     of this adapter, and it lands on the write milestone too.
#   - Granting the GitHub Actions runner this means code reaching that runner
#     executes as an administrator on a DHCP server. The trust boundary is still
#     "who can push to main" -- the workflow has no pull_request trigger -- but
#     what that code can do is now much larger. See the SECURITY note in
#     .github/workflows/windows-verify.yml.
#   - Prefer a dedicated account over NETWORK SERVICE. That identity is shared by
#     many Windows services, so granting it makes all of them administrators.
#
# If that trade is not acceptable, the alternative is switching the backend
# transport to netsh, which does respect DHCP Users. See docs/dhcp-backend.md.
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
    [string]$Account = 'S-1-5-20',

    # The GitHub Actions runner service. Group membership is stamped into an
    # access token at logon, so a running service keeps its old token and goes on
    # failing with WIN32 5 until it restarts.
    [string]$RunnerService = 'actions.runner.*',

    [switch]$SkipRunnerRestart,

    # Required. Granting administrator is not something to do by accident, so the
    # script refuses without it.
    [switch]$IUnderstandThisGrantsAdministrator
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

# --- consent ---------------------------------------------------------------

if (-not $IUnderstandThisGrantsAdministrator) {
    Write-Host ""
    Write-Host "This grants LOCAL ADMINISTRATOR to $Account on $env:COMPUTERNAME." -ForegroundColor Red
    Write-Host ""
    Write-Host "Not DHCP read-only -- full local administrator. The DHCP Users group looks"
    Write-Host "like the right answer and grants nothing to the PowerShell cmdlets; see the"
    Write-Host "measurements at the top of this file."
    Write-Host ""
    Write-Host "If that is what you want:"
    Write-Host "  .\grant-dhcp-access.ps1 -IUnderstandThisGrantsAdministrator"
    Write-Host ""
    Write-Host "If it is not, the alternative is switching the backend transport to netsh,"
    Write-Host "which does respect DHCP Users. See docs/dhcp-backend.md."
    exit 1
}

Write-Step "Host"
Write-Host "    computer : $env:COMPUTERNAME"
Write-Host "    running  : $($identity.Name)"

$role = (Get-CimInstance Win32_ComputerSystem).DomainRole

if ($role -eq 4 -or $role -eq 5) {
    Write-Host ""
    Write-Host "This host is a domain controller. Local groups do not exist here, and" -ForegroundColor Red
    Write-Host "NETWORK SERVICE cannot be added to an AD group -- on a DC it authenticates"
    Write-Host "as the computer account. Grant the equivalent through AD instead, and read"
    Write-Host "the notes at the top of this file first: the privilege involved is larger"
    Write-Host "than it looks."
    exit 1
}

# --- grant -----------------------------------------------------------------
#
# The built-in Administrators group resolved by its well-known SID, because the
# name is localized ("Administratoren" on a German host).

$group = (Get-LocalGroup -SID 'S-1-5-32-544').Name

Write-Step "Granting $Account membership of '$group'"
Write-Warn "This is local administrator, not DHCP read-only. See the header."

$existing = @(Get-LocalGroupMember -Group $group -ErrorAction SilentlyContinue)
$already  = $existing | Where-Object { $_.SID.Value -eq $Account }

if ($already) {
    Write-Ok "already a member, nothing to do"
} else {
    Add-LocalGroupMember -Group $group -Member $Account
    Write-Ok "added"
}

# --- restart the runner ----------------------------------------------------
#
# The load-bearing step. Group membership lands in an access token at logon, so
# the already-running service keeps the token it started with and goes on being
# denied. Skipping this is the single likeliest way to conclude the grant did
# not work.

if ($SkipRunnerRestart) {
    Write-Warn "Skipping the runner restart, as asked. The new membership does NOT apply"
    Write-Warn "to the running service until it restarts:"
    Write-Warn "  Get-Service $RunnerService | Restart-Service"
    exit 0
}

Write-Step "Restarting the runner so it picks up the new membership"

$runners = @(Get-Service $RunnerService -ErrorAction SilentlyContinue)

if ($runners.Count -eq 0) {
    # Reported rather than passed over in silence: an earlier version of this
    # printed "restarted" unconditionally, which cost a diagnostic round.
    Write-Warn "No service matching '$RunnerService' on this host -- nothing restarted."
    Write-Warn "The grant does not reach a process that is already running."
} else {
    $runners | Restart-Service
    Start-Sleep -Seconds 2
    $runners | Get-Service | Format-Table Name, Status -AutoSize
    Write-Ok "restarted"
}

Write-Host ""
Write-Host "Done. Re-run the Windows verify workflow." -ForegroundColor Green
