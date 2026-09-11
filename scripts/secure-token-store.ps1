# RETIRED. The adapter secures its own files now.
#
# Use this instead, from an ELEVATED PowerShell:
#
#     weave-adapter-dhcp-windows.exe service secure --config C:\path\to\config.toml
#
# `service install` already does it, so this is only for a console deployment,
# or after you move the token store or the log.
#
# ---------------------------------------------------------------------------
# WHY THIS SCRIPT WENT AWAY
# ---------------------------------------------------------------------------
#
# Its reasoning was right and is preserved in internal/core/winsvc/secure.go:
# NTFS ignores the 0o600 the adapter sets, the file's real protection is its
# ACL, and a WRITE is the actual risk -- anyone who can append a hash to the
# token store gets a bearer token the adapter accepts at its next start.
# SIDs rather than names, because a name is locale-dependent.
#
# Its DEFAULTS had drifted from decisions taken after it was written:
#
#   - it granted S-1-5-20 (NETWORK SERVICE). The service runs as LocalSystem,
#     S-1-5-18 -- forced by measurement, since the PowerShell DHCP cmdlets
#     gate on Administrators and NETWORK SERVICE was refused WIN32 5 on
#     WS2022 in every group tried.
#   - it defaulted to the relative path tokens.toml, which a service now
#     refuses outright: under the SCM the working directory is
#     C:\Windows\System32.
#   - it secured files only. The log file is created by the adapter at
#     runtime, so the LOG DIRECTORY needs inheritable entries or each new log
#     lands on whatever the parent permits.
#   - it knew nothing of the config file, which now carries
#     identity.namespaceKey.
#
# And a script has to be told the paths, while the binary has just resolved
# and validated all three. That is the same argument that made the installer
# a Go subcommand rather than a .ps1.

Write-Host "This script is retired. Run instead, from an elevated prompt:" -ForegroundColor Yellow
Write-Host ""
Write-Host "    weave-adapter-dhcp-windows.exe service secure --config <path-to-config.toml>" -ForegroundColor Cyan
Write-Host ""
Write-Host "`service install` already secures these files; this is for a console" -ForegroundColor Yellow
Write-Host "deployment, or after moving the token store or log." -ForegroundColor Yellow
exit 1
