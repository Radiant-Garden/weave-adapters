# Times the adapter's own probe query against the real DHCP backend.
#
# RUN THIS IN AN ELEVATED POWERSHELL ON THE DHCP HOST.
#
# It is read-only: it runs Get-DhcpServerv4Scope and nothing else. It changes
# no configuration, touches no service, and writes no file.
#
# ---------------------------------------------------------------------------
# WHAT IT IS FOR
# ---------------------------------------------------------------------------
#
# Two questions this repository cannot answer from a developer machine:
#
#   1. How long does a backend call actually take on this host? It is the input
#      the M3a cache decision is gated on, and docs/dhcp-backend.md lists it as
#      unverified.
#   2. How long does it take THREE MINUTES AFTER A BOOT? Both BACKEND-101 probe
#      timeouts seen on WS2022 landed in that window -- the delayed-start storm,
#      where Defender, Windows Update and WMI are all competing. Measured warm,
#      the same query takes about 0.67s against a 3s bound, so the steady-state
#      number does not explain them.
#
# The bound cannot answer question 2 by itself: a probe killed at its deadline
# reports the deadline, not how slow the call really was. So this measures
# WITHOUT a bound, which is the only way to turn "more than 3s" into a number.
#
# ---------------------------------------------------------------------------
# HOW TO GET THE NUMBER THAT MATTERS
# ---------------------------------------------------------------------------
#
#   Restart-Computer
#   <wait three minutes>
#   task measure-backend           # or: powershell -File scripts\measure-backend-latency.ps1
#
# Run 1 is reported separately because it is the only genuinely cold one: after
# it, PowerShell and the DhcpServer module are in the file cache and every
# later run is warm. Cold is the case that fails.

[CmdletBinding()]
param(
    # How many times to run the query. Thirty takes about 20 seconds warm and
    # is enough for a median and a rough tail.
    [int]$Iterations = 30,

    # The DHCP server to query. Empty means the local host, which is what
    # dhcp.server defaults to.
    [string]$Server = ''
)

$ErrorActionPreference = 'Stop'

# The adapter's probeScript, verbatim in spirit: same cmdlet, same projection,
# same -NoProfile -NonInteractive child process. What dominates the time is the
# process start, the DhcpServer module autoload and the WMI round trip, none of
# which the projection changes -- but keeping it identical means the number is
# the probe's, not an approximation of it.
$inner = @(
    "`$ErrorActionPreference = 'Stop'"
    '[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false'
    '$params = @{}'
    'if ($env:WADAPT_DHCP_SERVER) { $params[''ComputerName''] = $env:WADAPT_DHCP_SERVER }'
    '$scopeIds = Get-DhcpServerv4Scope @params | Select-Object @{n=''scopeId'';e={$_.ScopeId.IPAddressToString}}'
    'ConvertTo-Json -InputObject @{ scopes = @($scopeIds); psVersion = [string]$PSVersionTable.PSVersion } -Depth 5'
) -join '; '

$env:WADAPT_DHCP_SERVER = $Server

Write-Host "Querying $(if ($Server) { $Server } else { 'the local host' }), $Iterations iterations." -ForegroundColor Cyan

# One unmeasured run first, to fail loudly and legibly rather than timing an
# error thirty times. A permission failure here is WIN32 5 -- see
# docs/dhcp-backend.md, the cmdlets need local administrator.
$check = powershell.exe -NoProfile -NonInteractive -Command $inner 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Host 'The query FAILED. Timing it would measure nothing:' -ForegroundColor Red
    Write-Host $check
    exit 1
}

$uptime = (Get-Date) - (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
Write-Host ("Host booted {0:N1} minutes ago." -f $uptime.TotalMinutes) -ForegroundColor Cyan
if ($uptime.TotalMinutes -gt 10) {
    Write-Host 'NOTE: this is a warm host. The post-boot window is the one that produced the timeouts.' -ForegroundColor Yellow
}

$times = 1..$Iterations | ForEach-Object {
    (Measure-Command {
        powershell.exe -NoProfile -NonInteractive -Command $inner | Out-Null
    }).TotalMilliseconds
}

# Percentiles by order statistic. Crude, and fine: the question is whether the
# tail is near 700ms or near 8000ms, not where it sits to three digits.
$sorted = $times | Sort-Object
function Pick([double]$q) { [int]$sorted[[Math]::Min($sorted.Count - 1, [int][Math]::Floor($q * $sorted.Count))] }

Write-Host ''
Write-Host ("run 1 (coldest): {0} ms" -f [int]$times[0]) -ForegroundColor Green
[pscustomobject]@{
    n              = $sorted.Count
    min            = [int]$sorted[0]
    p50            = Pick 0.50
    p95            = Pick 0.95
    max            = [int]$sorted[-1]
    bootAgeMinutes = [int]$uptime.TotalMinutes
} | Format-List

Write-Host 'Compare against dhcp.probeTimeout (default 3s) and dhcp.commandTimeout (default 10s).' -ForegroundColor Cyan
