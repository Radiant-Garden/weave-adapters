# weave-adapters

> A clean, uniform REST API for **Windows Server DHCP**.

[![Status](https://img.shields.io/badge/status-1.0.0%20in%20progress-orange)](https://github.com/Radiant-Garden/weave-adapters/milestones)
[![Maintained](https://img.shields.io/badge/maintained-yes-brightgreen)](https://github.com/Radiant-Garden/weave-adapters/graphs/commit-activity)
[![CI](https://github.com/Radiant-Garden/weave-adapters/actions/workflows/ci.yml/badge.svg)](https://github.com/Radiant-Garden/weave-adapters/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/Radiant-Garden/weave-adapters/branch/main/graph/badge.svg)](https://codecov.io/gh/Radiant-Garden/weave-adapters)
[![Go Reference](https://pkg.go.dev/badge/github.com/radiantgarden/weave-adapters.svg)](https://pkg.go.dev/github.com/radiantgarden/weave-adapters)
[![Go Report Card](https://goreportcard.com/badge/github.com/radiantgarden/weave-adapters)](https://goreportcard.com/report/github.com/radiantgarden/weave-adapters)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

`weave-adapter-dhcp-windows` exposes Windows Server DHCP as a predictable,
versioned HTTP/JSON API — with ETags, conditional requests, machine-readable
errors, and health checks — so it's easy to automate, monitor, and integrate.

## Why

Windows DHCP is normally driven through PowerShell or RPC. This adapter puts a
clean REST API in front of it, so any generic HTTP client can read and manage
scopes, reservations, and leases the same way.

## Status

🟡 **Work toward `1.0.0` is in progress.** APIs may still change before the
1.0.0 tag.

## Quick start

```bash
# Build
task build            # or: go build ./cmd/weave-adapter-dhcp-windows

# Configure (all keys optional; flags > env WEAVE_ADAPTER_* > file > defaults)
cp config.example.toml config.toml

# Run
./weave-adapter-dhcp-windows --config config.toml
```

Or with Docker:

```bash
docker compose up
```

Check it's alive:

```bash
curl http://localhost:8444/api/v1/health
```

## Documentation

- [`docs/`](docs/) — how the project works, event catalog, and more.
- [`.claude/plans/`](.claude/plans/) — architecture, API conventions, and roadmap.
- API is spec-first OpenAPI (`api/<adapter>/openapi.yaml`).

## Maintainer & usage

Maintained by **[Radiant Garden](https://radiantgarden.io)**.

Built for **[weave](https://weave.radiantgarden.io)** — but **free for
individual use** under the [Apache 2.0 license](LICENSE).

## License

[Apache License 2.0](LICENSE) © Radiant Garden