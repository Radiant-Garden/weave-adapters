# Command reference

Everything `weave-adapter-dhcp-windows` accepts. The binary does two jobs: it
runs the adapter, and it manages the bearer tokens weave uses to authenticate
against it.

On Windows the binary is `weave-adapter-dhcp-windows.exe`; examples below drop
the extension for brevity.

```
weave-adapter-dhcp-windows [flags]              # run the adapter
weave-adapter-dhcp-windows token <command>      # manage tokens
```

## Running the adapter

```console
$ weave-adapter-dhcp-windows --port 8444
```

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--port` | int | `8444` | TCP port to listen on (1–65535) |
| `--config` | string | none | Path to a TOML config file |
| `--log-severity` | string | `info` | Log level: `debug`, `info`, `warn`, `error` |
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
| everything else | bearer | `401` without a valid token — including paths that match no route |

`/api/v1/health` returns `200` when healthy or unhealthy and `503` when
unavailable, so a readiness probe can key on the status code alone.

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

## Exit codes and output

| Code | Meaning |
|---|---|
| `0` | Success, including `--help` |
| `1` | Any failure |

Failures print differently depending on which job failed, and the difference is
deliberate:

- **Adapter startup** failures emit a structured `SYS-005` event, because they
  are operational outcomes an operator's log pipeline should capture.
- **Token command** failures print `error: <message>` to stderr. A duplicate
  label is a typo, not a startup failure, and does not belong in the event log.

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
