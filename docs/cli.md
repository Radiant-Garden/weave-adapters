# Command reference

Everything `weave-adapter-dhcp-windows` accepts. The binary does two jobs: it
runs the adapter, and it manages the bearer tokens weave uses to authenticate
against it.

On Windows the binary is `weave-adapter-dhcp-windows.exe`; examples below drop
the extension for brevity.

```
weave-adapter-dhcp-windows [flags]              # run the adapter
weave-adapter-dhcp-windows token <command>      # manage tokens
weave-adapter-dhcp-windows service <command>    # manage the Windows service
```

Started by the Windows Service Control Manager, the binary detects that and
runs as a service with no flag involved — see
[windows-service.md](windows-service.md).

## Running the adapter

```console
$ weave-adapter-dhcp-windows --port 8444
```

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--port` | int | `8444` | TCP port to listen on (1–65535) |
| `--config` | string | none | Path to a TOML config file |
| `--log-severity` | string | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-file` | string | none | Write the log here instead of stdout. **Required for a service:** the SCM discards stdout, so a service without it logs nowhere |
| `--disable-https` | bool | `true` | Must stay `true` — HTTPS is not implemented yet, so `false` is a startup error rather than a silent no-op |
| `--auth-tokens-file` | string | `tokens.toml` | Path to the bearer token store, read once at startup |
| `--disable-auth` | bool | `false` | Development only: serves every route unauthenticated, and says so loudly at startup (`SYS-006`) |

Configuration resolves **flags > environment > config file > defaults**.
Environment variables use the `WEAVE_ADAPTER_` prefix: `WEAVE_ADAPTER_PORT`,
`WEAVE_ADAPTER_LOG_SEVERITY`, `WEAVE_ADAPTER_DISABLE_HTTPS`. See
[`config.example.toml`](../config.example.toml) for a documented sample.

Startup fails if authentication is on and the token store is missing or empty;
the error names the command that fixes it. See
[token-management.md](token-management.md).

The adapter shuts down gracefully on Ctrl+C (`os.Interrupt`) or `SIGTERM`,
draining in-flight requests first.

### Endpoints

| Path | Auth | Purpose |
|---|---|---|
| `GET /api/v1/health` | none | Status, version, uptime, per-component detail |
| `GET /openapi.yaml` | none | The served API contract, as YAML |
| `GET /api/v1/scopes` | bearer | The server's IPv4 DHCP scopes: cursor-paginated, strong `ETag`, `?scopeId=` filter |
| everything else | bearer | `401` without a valid token — including paths that match no route |

`/api/v1/health` returns `200` when healthy or unhealthy and `503` when
unavailable, so a readiness probe can key on the status code alone.

`/api/v1/scopes` reads the DHCP server on every request — the read path is
stateless, so a walk of P pages costs P backend calls. A failure there is `502`
(unreachable, or unusable output) or `504` (timed out), never `500`: the adapter
is a gateway, and a `500` would claim the adapter itself is broken. See
[dhcp-backend.md](dhcp-backend.md).

## Token management

Full background — how tokens are generated, stored, and rotated — is in
[token-management.md](token-management.md). This section is the flag reference.

All token commands accept `--file` (default `tokens.toml`, resolved relative to
the working directory) and support `--help`.

### `token gen`

Mints a token, stores its hash, and prints the token once.

```console
$ weave-adapter-dhcp-windows token gen --label weave-prod
```

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--label` | string | *(required)* | Identifies the token; becomes the caller subject in logs |
| `--file` | string | `tokens.toml` | Path to the token store |
| `--expires-in-days` | int | `0` | Days until the token stops being accepted; `0` never expires |

Labels are 1–64 characters of letters, digits, `-` or `_`, starting with a
letter or digit. A label that already exists is rejected — `gen` never
overwrites, because overwriting would silently revoke a token still in use.

### `token list`

Shows configured tokens. Output contains no tokens and no hashes, so it is safe
to paste into a ticket.

```console
$ weave-adapter-dhcp-windows token list
LABEL          CREATED     EXPIRES     STATUS
weave-prod     2026-07-18  never       active
weave-staging  2026-07-18  2026-10-16  expires in 90 days
weave-old      2026-01-02  2026-07-16  EXPIRED 2 days ago
```

A missing token file is reported as "No tokens configured", not an error — a
fresh install simply has none yet.

### `token revoke`

Removes a token by label.

```console
$ weave-adapter-dhcp-windows token revoke --label weave-staging
```

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--label` | string | *(required)* | Label of the token to remove |
| `--file` | string | `tokens.toml` | Path to the token store |

An unknown label is an error, so a typo can never look like a successful
revocation while the real token stays live.

## Windows service management

Every command here needs an **elevated** prompt — the Service Control Manager
refuses all of them to a non-administrator — and every one of them operates on
the fixed service name `wadapt-dhcp-windows`.

```console
$ weave-adapter-dhcp-windows service install --config C:\ProgramData\weave-adapters\config.toml --i-understand-this-runs-as-localsystem
$ weave-adapter-dhcp-windows service start
$ weave-adapter-dhcp-windows service status
$ weave-adapter-dhcp-windows service stop
$ weave-adapter-dhcp-windows service uninstall --yes
```

| Command | Does |
|---|---|
| `install` | Registers the service, sets its recovery behaviour, and locks down its files |
| `uninstall` | Stops it, deletes the registration, removes its Event Log source. Needs `--yes` |
| `start` / `stop` | Starts or stops it, waiting for the transition to finish |
| `status` | What the SCM knows: state, start type, registered path, drain budget, recovery |
| `secure` | Re-applies the file lockdown without touching the registration |

### `service install`

| Flag | Required | Meaning |
|---|---|---|
| `--config` | **yes** | Absolute path to the TOML config file |
| `--i-understand-this-runs-as-localsystem` | **yes** | Acknowledges the privilege grant |

`--config` is mandatory rather than optional, and the reason is worth
understanding before you work around it. `identity.namespaceKey` has no
command-line flag by design — an argv entry is readable by any local user —
and a service has no private environment either: both the machine-wide
variables and a service's own `Environment` value live in registry locations
every local account can read. **The config file is the only channel left**, so
a service installed without one cannot start at all.

Install refuses a configuration that would not start, rather than registering
it and letting the SCM retry three times before anyone looks. It resolves the
file **without the process environment** — the elevated shell you are typing
in is not the environment the service will get — and rejects:

- any relative path (`authTokensFile`, `logFile`, `dhcp.powershellPath`), since
  a service resolves them against `C:\Windows\System32`;
- anything the server itself would reject, a missing `identity.namespaceKey`
  most often.

See [windows-service.md](windows-service.md) for what it registers and why.

### `service uninstall`

```console
$ weave-adapter-dhcp-windows service uninstall
This stops wadapt-dhcp-windows, deletes its registration, and removes its Event Log source.
Re-run with --yes to proceed.
```

### `service status`

```console
$ weave-adapter-dhcp-windows service status
wadapt-dhcp-windows
  state:        Running
  start type:   automatic
  binary:       "C:\Program Files\weave-adapters\weave-adapter-dhcp-windows.exe" --config "C:\ProgramData\weave-adapters\config.toml"
  drain budget: 20s
  recovery:     restarts on failure, including a clean non-zero exit
```

The last line is the one to read. If it says **`WILL NOT restart on a clean
non-zero exit`**, the restart schedule is registered but inert: Windows runs
failure actions only for a process that dies *without* reporting stopped,
which is not how a configuration failure exits. Reinstall to fix it.

### `service secure`

```console
$ weave-adapter-dhcp-windows service secure --config C:\ProgramData\weave-adapters\config.toml
```

Locks the config file, the token store and the **log directory** to `SYSTEM`
and the local `Administrators` group, and nothing else. `install` does this
already; run it again after `token gen` creates a store that did not exist at
install time, or after moving either path.

## Exit codes and output

| Code | Meaning |
|---|---|
| `0` | Success, including `--help` |
| `1` | Any failure |

Failures print differently depending on which job failed, and the difference is
deliberate:

- **Adapter startup** failures emit a structured `SYS-005` event, because they
  are operational outcomes an operator's log pipeline should capture.
- **Token and service command** failures print `error: <message>` to stderr. A
  duplicate label or a forgotten `--config` is a typo, not a startup failure,
  and does not belong in the event log.

```console
$ weave-adapter-dhcp-windows token gen --label weave-prod
error: a token with that label already exists: "weave-prod"

$ weave-adapter-dhcp-windows --port 70000
ERROR startup failed eventId=SYS-005 data.error="loading config: port must be between 1 and 65535, got 70000"
```

## Development

Local builds go through [Task](https://taskfile.dev):

```console
$ task build            # host binary → bin/
$ task build-windows    # windows/amd64 → bin/*.exe
$ task run              # build and run
$ task test             # -race -shuffle=on
$ task ci               # the full gate; run before pushing
```
