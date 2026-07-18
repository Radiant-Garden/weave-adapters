# weave-adapters

> A clean, uniform REST API for **Windows Server DHCP**.

[![Status](https://img.shields.io/badge/status-1.0.0%20in%20progress-orange)](https://github.com/Radiant-Garden/weave-adapters/milestones)
[![Maintained](https://img.shields.io/badge/maintained-yes-brightgreen)](https://github.com/Radiant-Garden/weave-adapters/graphs/commit-activity)
[![CI](https://github.com/Radiant-Garden/weave-adapters/actions/workflows/ci.yml/badge.svg)](https://github.com/Radiant-Garden/weave-adapters/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/Radiant-Garden/weave-adapters/branch/main/graph/badge.svg)](https://codecov.io/gh/Radiant-Garden/weave-adapters)
[![Go Reference](https://pkg.go.dev/badge/github.com/radiantgarden/weave-adapters.svg)](https://pkg.go.dev/github.com/radiantgarden/weave-adapters)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)
[![EU Software](https://img.shields.io/badge/🇪🇺-EU_Software-003399)](https://radiantgarden.io)

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

## Documentation

🚧 **Work in progress.**

- [`docs/cli.md`](docs/cli.md) — command and flag reference.
- [`docs/token-management.md`](docs/token-management.md) — how weave
  authenticates, and how tokens are generated, stored, and rotated.
- [`docs/events.md`](docs/events.md) — the generated event catalog.
- [`docs/how-we-work.md`](docs/how-we-work.md) — branching, commits, local dev.
- API is spec-first OpenAPI (`api/<adapter>/openapi.yaml`).

## Maintainer

Maintained by **[Radiant Garden](https://radiantgarden.io)**, built for
**[weave](https://weave.radiantgarden.io)**.

## License

Licensed under the [Apache License 2.0](LICENSE) — free to use, modify, and
distribute, including commercially.