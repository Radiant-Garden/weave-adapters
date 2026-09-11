# Running the adapter as a Windows service

How to install, operate and troubleshoot `weave-adapter-dhcp-windows` under
the Windows Service Control Manager: what the install registers, why it asks
for the things it asks for, and where to look when it will not start.

Every command here needs an **elevated** prompt. The command reference is in
[cli.md](cli.md); this is the deployment and the reasoning behind it.

---

## Before you install

Three things have to be true, and each will otherwise fail in a way that
looks like something else.

### 1. A config file, at an absolute path

```toml
# C:\ProgramData\weave-adapters\config.toml
port           = 8444
logFile        = 'C:\ProgramData\weave-adapters\adapter.log'
authTokensFile = 'C:\ProgramData\weave-adapters\tokens.toml'

[identity]
namespaceKey = '<provisioned once, backed up, never rotated casually>'
serverName   = 'dhcp01.example.internal'

[dhcp]
# powershellPath = 'powershell.exe'   # a bare name is fine; a relative path is not
```

`--config` is required, and the reason is not convenience.
`identity.namespaceKey` has no command-line flag — an argv entry is readable
by any local user, and the key is backup-critical — and a service has no
private environment either: both machine-wide variables and a service's own
`Environment` value live in registry locations every local account can read.
**The config file is the only channel.**

Every path must be **absolute**. Under the SCM the working directory is
`C:\Windows\System32`, so a relative path resolves somewhere nobody intended
and the service fails at startup with a file-not-found naming a path you can
see exists. Install rejects them rather than letting you discover that at 3am.

A **bare command name** is the one exception: `powershellPath = 'powershell.exe'`
is fine, because Windows resolves it through `PATH` and never through the
working directory. `bin\pwsh.exe` is not.

### 2. `logFile` set

The SCM discards stdout. A service without `logFile` runs correctly and logs
nowhere. The critical events also go to the Event Log (below), but the file is
the complete stream.

The log is opened for **append** — a restart does not erase the run that
prompted it — and it **grows without bound**. There is no rotation yet; size
it or rotate it externally.

### 3. A token, or a deliberate decision not to have one

```console
$ weave-adapter-dhcp-windows token gen --label weave-prod --file C:\ProgramData\weave-adapters\tokens.toml
$ weave-adapter-dhcp-windows service secure --config C:\ProgramData\weave-adapters\config.toml
```

The store does not have to exist at install time — install says so rather than
claiming it secured a file that was not there — but **re-run `service secure`
after creating it**, or the store keeps whatever ACL it inherited.

---

## Installing

```console
$ weave-adapter-dhcp-windows service install ^
    --config C:\ProgramData\weave-adapters\config.toml ^
    --i-understand-this-runs-as-localsystem
$ weave-adapter-dhcp-windows service start
```

### Why LocalSystem, and why you have to say so

The service runs as **LocalSystem**, the most privileged account on the host.
That is not a default nobody revisited — it was measured on Windows Server
2022, and the alternatives do not work:

| Account | Group | Result |
|---|---|---|
| `NETWORK SERVICE` | DHCP Users | refused, `WIN32 5` |
| `NETWORK SERVICE` | DHCP Administrators | refused, `WIN32 5` |
| an ordinary user | DHCP Users | refused, `WIN32 5` |
| LocalSystem | (local administrator) | works |

Windows offers a "DHCP Users" group described as read-only access to the DHCP
service, and it grants the PowerShell cmdlets nothing: they go through WMI,
which gates on Administrators. A service account that cannot read DHCP serves
`503` from every endpoint. See [dhcp-backend.md](dhcp-backend.md).

The consent flag exists because a privilege grant should not happen because
somebody ran a command that sounded routine.

**The escape hatch**, if LocalSystem is unacceptable in your environment, is
the `netsh` transport described in [dhcp-backend.md](dhcp-backend.md). It
respects `DHCP Users`, at the cost of parsing tabular text.

> ⚠️ **TLS is not implemented.** `disableHttps` must stay `true`, so the bearer
> token weave sends crosses the wire in clear. A service that starts at boot
> and survives logoff makes it more likely somebody points real traffic at it.
> This deployment is lab-only until TLS lands.

### What install registers

| Setting | Value | Why |
|---|---|---|
| Start type | Automatic, **not** delayed | Measured: the local DHCP Server service reaches Running about **4 seconds** after we do. Delaying would trade a 4-second window where health honestly answers 503 for ~2 minutes with no adapter at all |
| Recovery | Restart after 5s, 10s, 60s; counter resets after 24h | Widening, so a transient cause clears early while a real one does not loop every 5 seconds forever. The reset stops the last interval becoming permanent |
| Recovery on clean failure | **Enabled** | Without this flag Windows runs failure actions only for a process that dies *without* reporting stopped — which is not how a config failure exits, so the schedule would be registered and inert |
| `PreshutdownTimeout` | The drain budget **plus margin** | A hard wall, not something progress extends. Set to the deadline rather than the budget so the SCM does not stop waiting at the moment a full-length drain reports stopped |
| Dependencies | **none** | A hard dependency on `DHCPServer` would make the service un-startable on a host targeting a remote server through `dhcp.server` |
| File ACLs | SYSTEM + Administrators, inheritance off | See below |

### What install locks down

Three targets, to `SYSTEM` (`S-1-5-18`) and the local `Administrators` group
(`S-1-5-32-544`) only, with inherited entries detached:

- **the config file** — it carries `identity.namespaceKey`. A read leaks what
  every `wadaptID` on this host derives from; a write re-keys the fleet.
- **the token store** — a read leaks only hashes, which cannot be replayed. A
  **write** is a local privilege escalation into the API: anyone who can
  append a hash gets a token the adapter accepts at its next start.
- **the log directory**, not the log file. The adapter creates the log at
  runtime, and a new file inherits its parent's entries — securing the file
  would leave the next one on the directory's defaults.

SIDs rather than names throughout, because a name is locale-dependent: on a
German host the administrators group is `Administratoren`.

The service **refuses to start** if any of them grants write to anyone else.
It reports the offending SID and names `service secure`; it does not repair
them itself, because a service rewriting its own ACLs at boot would quietly
undo a deliberate change — and would need write access to the descriptor it
is protecting.

---

## Where the output goes

### The log file

Everything, in the text format the console run uses. This is the complete
stream.

### The Windows Event Log

A deliberate subset, under **Windows Logs → Application**, source
`wadapt-dhcp-windows`. It exists because a file sink cannot promise anything
about a failure that happened *before* the file was opened — which is exactly
when a service will not start.

What reaches it is a rule, not a list: every event at **WARN or above** that
is not triggered by an inbound request, plus the two startup anchors. The
request exclusion is what stops a client's 404 storm filling the Application
log.

| Event ID | Catalog ID | Meaning |
|---|---|---|
| 1 | `SYS-001` | Started. Carries `runMode=service` |
| 2 | `SYS-002` | Listening, with the address |
| 5 | `SYS-005` | **Failed to start**, with the reason |
| 6 | `SYS-006` | Authentication is disabled |
| 7 | `SYS-007` | The drain was cut short |
| 111 | `API-011` | A handler panicked |
| 201 | `HLT-001` | Health changed state |
| 501 | `BACKEND-101` | A DHCP backend call failed |
| 502 | `DHCP-002` | A scope changed materially under an existing wadaptID |

These numbers are what an Event Viewer filter matches on, so they are a
contract: they are never renumbered. The authoritative list is
[events.md](events.md), which carries a **Windows Event Log ID** line on every
mirrored event.

```powershell
Get-WinEvent -ProviderName wadapt-dhcp-windows -MaxEvents 30 |
  Select-Object TimeCreated, Id, Message | Format-List

# just the startup failures
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='wadapt-dhcp-windows'; Id=5}
```

---

## Troubleshooting

### `service start` fails immediately

Read the error: it names the exit code and points at Event Viewer. A
**service-specific** code means the adapter ran and reported its own failure,
so `SYS-005` is in the Event Log with the reason. A **Win32** code means it
never got that far.

```powershell
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='wadapt-dhcp-windows'; Id=5} -MaxEvents 5 |
  Format-List TimeCreated, Message
```

The usual causes, in the order worth checking: a path in the config that is
not absolute, a token store that is missing or empty, and a file whose ACL
grants write to someone outside SYSTEM and Administrators.

### It starts, then keeps restarting

That is the recovery schedule doing its job on a configuration that fails
every time. Each attempt writes `SYS-005`. Fix the cause, and the next
scheduled restart succeeds — you do not need to reinstall.

### `/api/v1/health` answers 503

Expected on any host without a reachable DHCP backend, and honest rather than
broken. Check the `dhcp-server` component's `detail`: it carries the shell's
error and the PowerShell version, which is usually enough. For the first few
seconds after a reboot it is also simply the DHCP Server service still coming
up.

### `service uninstall` says it worked, `install` then fails

`ERROR_SERVICE_MARKED_FOR_DELETE` (1072). Windows removes a service only once
the last open handle closes, so an open `services.msc` keeps a deleted name
alive. Close it and retry.

### The service will not stop

Check the drain. A stop waits for in-flight requests, bounded by the drain
budget `service status` reports. If it times out, `SYS-007` is in the log and
the requests were cut off.

---

## Uninstalling

```console
$ weave-adapter-dhcp-windows service uninstall --yes
```

Stops the service, deletes the registration, and removes the Event Log source.
It does **not** delete the config file, the token store or the log — those are
yours, and the namespace key in particular is backup-critical. The ACLs stay
as they were.

---

## What this deployment does not do

- **No TLS.** See the warning above. Lab-only until it lands.
- **No log rotation.** The file grows; rotate it externally.
- **No MSI or package.** Copy the binary and run `service install`.
- **No automatic ACL repair.** The service reports and refuses; `service
  secure` fixes.
