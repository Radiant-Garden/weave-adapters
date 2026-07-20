# The Windows DHCP backend

How `weave-adapter-dhcp-windows` talks to Windows Server DHCP, and the
PowerShell behaviours that shape every script in
`internal/adapters/dhcpwindows/`.

This exists because the knowledge below is expensive to rediscover: each item
was either verified against a live WS2022 host or caused a bug that took a while
to see. If you are changing a script in that package, read this first.

## Transport

`powershell.exe -NoProfile -NonInteractive -Command <script>`, via
`exec.CommandContext`, behind an injectable runner so the package stays testable
on macOS and Linux.

- **`-NoProfile` is load-bearing.** A profile script on the host writes to
  stdout, and stdout is the JSON we parse.
- **Execution policy is irrelevant**, because it governs script *files* and this
  passes an inline `-Command`. Do not "fix" that by switching to `-File`.
- **`cmd.WaitDelay` is set.** `exec.CommandContext` kills the process when the
  context expires, but `Wait` still blocks until the stdout pipe closes, and an
  inherited handle in a wedged child holds it open indefinitely. Without
  `WaitDelay` the timeout does not actually bound the call — and the health
  probe's separate, shorter timeout exists precisely so a hung shell cannot
  serialize health polling behind a mutex. The timeout only delivers on that if
  `Wait` is bounded too.
- **PowerShell 5.1 is the target; 7 is not required.** `dhcp.powershellPath`
  points at `pwsh` if an operator wants it. Everything below is written to work
  on both, which is why no script uses `-AsArray`, ternaries, `??`,
  `ForEach-Object -Parallel` or `$PSStyle`.

Requiring PS 7 would buy little and risk something: it loads some Windows
modules through the WinCompat shim, which proxies to a background 5.1 process
and returns **deserialized** objects with methods stripped. Preferring
properties over methods in projections (`.IPAddressToString`, `[string]$_.State`
rather than `.ToString()`) keeps us correct either way.

## Every script opens with the same two lines

```powershell
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
```

Neither is optional, and neither is version-specific.

**`Stop`** — the default is `Continue`, which means a permissions failure on
`Get-DhcpServerv4Scope` writes to stderr, leaves the pipeline empty, **exits
zero**, and serializes to an empty result. We would decode that as "this server
has zero scopes" and answer a cheerful `200` with an empty list; weave would see
a healthy adapter reporting no scopes and could reasonably conclude they had all
been deleted. A silent wrong answer is worse than a crash.

**The encoding line** — when PS 5.1's stdout is a pipe rather than a console it
encodes using `[Console]::OutputEncoding`, which defaults to the OEM code page
(437/850). A scope named `Standort München` arrives as mojibake, and Go's
`encoding/json` substitutes U+FFFD for invalid UTF-8 **rather than erroring**,
so it decodes "successfully" and we serve a corrupted name. `New-Object
System.Text.UTF8Encoding $false` rather than `[System.Text.Encoding]::UTF8`: the
latter carries a BOM preamble, and a BOM at the head of stdout breaks the JSON
decode.

## The serialization traps

Never pipe cmdlet output straight into `ConvertTo-Json`. Every command projects
explicitly with `Select-Object` first, to flat strings and ints. Six reasons,
all verified:

| # | Behaviour | What it produces | Guard |
|---|---|---|---|
| 1 | `-Depth` defaults to **2** | nested values become the literal string `"System.Object[]"` | pass `-Depth` explicitly |
| 2 | `System.Net.IPAddress` serializes as an **object** | `{"Address":…,"AddressFamily":…}` where you wanted `"10.0.0.0"` | `.IPAddressToString` on every address field |
| 3 | `TimeSpan` serializes as `{Ticks, Days, Hours, …}` | an object where the convention is integer seconds | `[int]$_.LeaseDuration.TotalSeconds` |
| 4 | A **single-element** result does not serialize as an array | a server with exactly one scope emits a bare object and breaks the decoder | `ConvertTo-Json -InputObject @($result)` — PS 5.1 has no `-AsArray` |
| 5 | The cmdlet returns **CimInstance** objects | `CimClass`, `CimInstanceProperties`, `CimSystemProperties`, `PSComputerName` dominate the payload | the explicit projection drops them |
| 6 | Redirected stdout uses the **OEM code page** | mojibake that decodes without error | the encoding line above |

Trap 6 is the dangerous one: it is the only entry here that walks straight
through every other guard. Keep the non-ASCII fixture in the test suite.

The projection also names every field explicitly, even where the name would pass
through unchanged — a bare `name, description` emits `Name`/`Description` and
breaks the camelCase convention on two fields while the other eight comply. The
Go struct tags in `scope.go` mirror it field for field.

## Decoder rules

- **Empty stdout is an error, not an empty list.** A DHCP server with no scopes
  emits a valid `[ ]` (verified — the exact bytes are `[\n\n]`), so the only
  remaining way to get zero bytes is a failure: a killed process, a crashed
  shell, output swallowed by a profile. Both guards are needed and neither
  subsumes the other — `Stop` catches the failures that still exit zero, this
  catches the ones that produce no output at all.
- **A bare `null` is not an empty list either.** `json.Unmarshal("null", &s)`
  leaves a nil slice and returns no error, so it walks past the empty-stdout
  check (the text is not empty) and past the per-element loop (there are no
  elements) to be served as "zero scopes" — the same wrong answer through a
  different door.
- **`scopeId` is validated as an IPv4 address on decode.** This is the Go-side
  tripwire for trap 1: `"System.Object[]"` decodes cleanly into a string field
  and derives a perfectly valid-looking `wadaptID`. So does a `[null]` element,
  which would otherwise become a phantom scope that exists nowhere and that
  weave would reconcile against.
- **stderr is context, not a verdict.** Under `-NoProfile -NonInteractive` with
  `Stop` it is *probably* clean, but PS 5.1 renders several streams in ways that
  surprise, and if anything benign ever landed there every request would fail.
  Non-zero exit and decode failure are the errors; stderr is attached to them.
  Promote it to an error of its own only once a real host has shown it silent
  across a successful run.

## Errors and what a client sees

The client returns four typed errors, and the scopes handler maps each to a
distinct status so the difference survives to weave — whose response classifier
reads the status code and never decodes the body.

| Error | Status | Problem type | Means |
|---|---|---|---|
| `ErrBackendUnavailable` | `502` | `backend-unavailable` | shell would not run, or exited non-zero |
| `ErrBackendTimeout` | `504` | `backend-timeout` | exceeded `dhcp.commandTimeout` |
| `ErrBackendMalformed` | `502` | `backend-error` | exited zero, output unusable |
| `ErrDuplicateWadaptID` | `500` | `internal` | two scopes derived one identity |

`502` rather than `500` because the adapter is a gateway: a `500` claims the
adapter itself is broken and sends an operator to the wrong logs. The duplicate
case is genuinely ours — the backend answered correctly and our derivation
collided — so it keeps the `500`.

**Raw stderr never reaches a response.** It can name internal hosts and paths.
It reaches the operator through `BACKEND-101`, which carries the shell's own
message; the client gets the curated `ResponseDetail`.

Each failed request therefore logs **two** lines by design: `BACKEND-101` at
ERROR with the cause, and `BACKEND-102`/`103`/`104` at WARN recording what the
client was told. See `docs/events.md`.

## Security

**M3a interpolates no value into any script.** The read commands take no
parameters and are Go constants. The one value they need — the server name —
arrives through the child process environment (`exec.Cmd.Env`, read as
`$env:WADAPT_*`) and is splatted onto the cmdlet.

**That is the binding rule for every future script, including mutations.** No
quoting, no injection surface, no temp file, it works with `-Command`, and
execution policy stays irrelevant. An earlier draft of the plan specified
`param()` blocks via `-ArgumentList`, which is not implementable:
`powershell.exe -Command` has no `-ArgumentList` (that belongs to
`Invoke-Command`/`Start-Process`/`Start-Job`), and getting a real `param()`
block requires `-File`, which makes execution policy relevant again.

The adapter is also **backend-read-only**: a read-only service account serves
every endpoint, because identity is derived rather than written back.

## Verified against a live WS2022 host

Single-scope test server, 2026-07-18.

| Assumption | Result |
|---|---|
| WS2022 with in-box PS 5.1 | ✅ `5.1.20348.558` (build 20348 is Server 2022) |
| `DhcpServer` module present | ✅ version `2.0.0.0` |
| Single-element array workaround holds | ✅ `ConvertTo-Json -InputObject @(…)` emitted brackets |
| `ScopeId` etc. are `IPAddress` | ✅ `.IPAddressToString` → `"192.168.178.0"` |
| Cmdlet returns plain objects | ❌ **CimInstance** — trap 5 |
| `LeaseDuration` survives as `TimeSpan` | ✅ `[int]…TotalSeconds` → `691200` |
| `State` / `Type` cast cleanly | ✅ `[string]` → `"Active"` / `"Dhcp"` |
| Full projection round-trips | ✅ ten flat fields, no CIM leakage |

**Still unverified**, and worth closing at sign-off:

- The non-ASCII fixture is hand-written, not captured from the host. It guards
  the Go decode side; it cannot prove the `[Console]::OutputEncoding` line works
  against a real OEM-code-page host. Capture a scope with an umlaut in its name
  and description.
- Whether stderr is silent across a *successful* run.
- `ListScopes` latency at a representative scope count, which is the input the
  cache decision is gated on.
