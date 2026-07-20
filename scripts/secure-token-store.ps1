# Locks down the bearer token store (tokens.toml) so only the adapter's account
# and administrators can read or write it.
#
# RUN THIS ONCE, IN AN ELEVATED POWERSHELL, ON THE ADAPTER HOST -- after
# `token gen` has created the file, and again if you ever move the store.
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
# ---------------------------------------------------------------------------
#
# The adapter writes tokens.toml with a 0o600 file mode. On Windows that mode is
# a no-op: NTFS ignores POSIX permission bits, and the file's real protection is
# its access-control list, which by default inherits from the parent directory
# -- often Users:Read, sometimes worse.
#
# The file stores only token HASHES, so a READ leaks nothing usable: a hash
# cannot be replayed as a bearer token. A WRITE is the actual risk. Anyone who
# can write the file can append their OWN token hash, and the adapter -- which
# reads the store once at startup and trusts every hash in it -- would then
# accept their token as a valid credential. That is a local privilege escalation
# into the adapter's API, and the default inherited ACL can permit it.
#
# This script replaces the inherited ACL with an explicit one: full control for
# SYSTEM, the local Administrators group, and the account the adapter runs as,
# and nobody else. It is the ACL companion to the 0o600 the adapter sets on
# every other platform.
#
# KEEP THIS FILE ASCII-ONLY. Windows PowerShell 5.1 decodes a BOM-less .ps1
# using the host's ANSI codepage; on a German-locale host that is CP1252, where
# the third byte of a UTF-8 em dash lands on a character PowerShell honours as a
# closing double quote, silently ending the string it sits in. Use "--", never
# an em dash.

[CmdletBinding()]
param(
    # The token store to secure. Defaults to the same relative path the adapter
    # and the token CLI default to (config.DefaultAuthTokensFile).
    [string]$Path = 'tokens.toml',

    # The account the adapter service runs as, granted read/write alongside
    # SYSTEM and Administrators. A SID is invariant across locales; a name is
    # not. NETWORK SERVICE (S-1-5-20) is the default a stock service uses, but a
    # dedicated account is preferred -- see grant-dhcp-access.ps1.
    [string]$ServiceAccount = 'S-1-5-20'
)

$ErrorActionPreference = 'Stop'

function Write-Step { param([string]$Text) Write-Host "==> $Text" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Text) Write-Host "    OK: $Text" -ForegroundColor Green }

# --- elevation -------------------------------------------------------------

$identity  = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)

if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "This script rewrites a file ACL, which needs Administrator." -ForegroundColor Red
    Write-Host "Re-run it from an elevated PowerShell (Run as administrator)." -ForegroundColor Red
    exit 1
}

# --- locate the file -------------------------------------------------------

if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
    Write-Host "No token store at '$Path'." -ForegroundColor Red
    Write-Host "Create one first: .\weave-adapter-dhcp-windows.exe token gen --label <name>" -ForegroundColor Red
    Write-Host "then re-run this against the file it wrote." -ForegroundColor Red
    exit 1
}

$full = (Resolve-Path -LiteralPath $Path).Path

Write-Step "Securing $full"

# --- build the explicit ACL ------------------------------------------------
#
# Well-known SIDs, because the group names are localized ("Administratoren",
# "SYSTEM" varies) and a name-based rule would not resolve on a non-English host.

$system         = New-Object Security.Principal.SecurityIdentifier('S-1-5-18')       # LOCAL SYSTEM
$administrators = New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')   # BUILTIN\Administrators
$service        = New-Object Security.Principal.SecurityIdentifier($ServiceAccount)

$acl = New-Object Security.AccessControl.FileSecurity

# Turn OFF inheritance and do NOT copy the inherited entries across: a protected
# ACL with only the three rules below is the whole point. Passing $false as the
# second argument is what drops the inherited Users:Read that the default grants.
$acl.SetAccessRuleProtection($true, $false)

foreach ($sid in @($system, $administrators, $service)) {
    $rule = New-Object Security.AccessControl.FileSystemAccessRule(
        $sid,
        [Security.AccessControl.FileSystemRights]::FullControl,
        [Security.AccessControl.AccessControlType]::Allow)
    $acl.AddAccessRule($rule)
}

# The owner should be Administrators, not whoever happened to create the file:
# an owner can always rewrite the ACL, so leaving it as a low-privileged creator
# would undo everything above.
$acl.SetOwner($administrators)

Set-Acl -LiteralPath $full -AclObject $acl

Write-Ok "ACL replaced: SYSTEM, Administrators, and $ServiceAccount have full control; nobody else."

# --- show the result -------------------------------------------------------

Write-Step "Effective permissions now"
(Get-Acl -LiteralPath $full).Access |
    Format-Table IdentityReference, FileSystemRights, AccessControlType -AutoSize

Write-Host ""
Write-Host "Done. Restart the adapter only if you also rotated tokens; the ACL change" -ForegroundColor Green
Write-Host "alone needs no restart -- it takes effect on the next open." -ForegroundColor Green
