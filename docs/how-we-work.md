# How we work in this repo

A short guide to the conventions we follow in `weave-adapters` — branching,
commit messages, and local development. If you're contributing or just reading
along, this is how things are done here.

## Branching & releases

We use **trunk-based development**: work lands directly on `main`. No long-lived
feature branches, no PR ceremony for routine changes.

Releases are cut by pushing a **version tag** (`vX.Y.Z`), which triggers the
release workflow in CI. `main` is always the source of truth.

## Commit messages

We follow the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
structure with our own type vocabulary.

```
<type>(<scope>)!: <summary>

<optional body>

<optional footers>
```

### Summary vs. body

- The **summary** (first line) is a short, imperative label — aim for ≤ ~50
  characters, one idea, no trailing period, no lists.
- Everything else — the *what* and *why*, file lists, context — goes in the
  **body**, one blank line below the summary.

```
infra: add Go tooling and CI

Taskfile, golangci-lint config, and GitHub Actions for the push/PR
quality gate plus a tag-triggered release workflow.
```

### Types

| Type | Use for | Release |
|---|---|---|
| `feat` | new feature | MINOR |
| `fix` | bug fix | PATCH |
| `vul` | security fix (any security-involved bug is `vul`, never `fix`) | PATCH |
| `perf` | same output, faster/leaner | PATCH |
| `ref` | refactor — code changed, **no observable output change** | — |
| `cng` | output changed but it's not a new feature (e.g. reordered CLI output) | — |
| `del` | deleted code (`del!` if it removes a public API/endpoint) | — |
| `docs` | documentation | — |
| `test` | tests | — |
| `infra` | tooling & infra files (Taskfile, Dockerfile, compose, CI). Dependency bumps use `infra(deps)` | — |
| `misc` | anything else | — |

`ref` vs `cng`: if the observable output is identical, it's `ref`; if the output
changed but you didn't add a feature, it's `cng`.

### Scope

Optional but encouraged in this monorepo — name the area touched, e.g.
`feat(core/config)`, `fix(dhcp-windows)`, `infra(deps)`.

### Breaking changes

This repo *is* an API contract that weave consumes, so flag anything that changes
that contract. Add `!` after the type/scope and/or a `BREAKING CHANGE:` footer —
either one makes it a **MAJOR** release.

```
feat(dhcp-windows)!: rename lease field for the weave contract

BREAKING CHANGE: leases[].mac renamed to leases[].hardwareAddress
```

### Linking issues (mind the auto-close)

Keep issue numbers **out of the summary** and put them in footers:

- `Closes: #12` — intentionally closes the issue.
- `Refs: #12` — links only, never closes.

Why: GitHub auto-closes an issue when a close-keyword (`close(s/d)`, `fix(es/ed)`,
`resolve(s/d)`) directly precedes a `#number` on the default branch — and we
commit straight to `main`, so it fires on push. A summary like `fix: #12 …` would
close #12 even if you only meant to reference it. Footers make the intent
explicit.

## Local development

**Prerequisites:** Go 1.26+, [Task](https://taskfile.dev), `golangci-lint` v2,
and Docker (for the local container).

Common tasks (`task --list` shows them all):

| Command | Does |
|---|---|
| `task run` | Run the adapter from source |
| `task build` | Build the host binary into `bin/` |
| `task build-windows` | Cross-compile the `windows/amd64` binary (the production target) |
| `task test` | Run tests with the race detector |
| `task check` | Format, vet, and lint |
| `task ci` | Full local gate — mirrors the CI pipeline |
| `task up` / `task down` | Start / stop the adapter in Docker |
| `task logs` | Follow the container logs |

The adapter currently serves `GET /api/v1/health` on port `:8444`.

### Docker

`task up` cross-compiles a static `linux/arm64` binary on the host and copies it
into an `ubuntu` image (the same pattern weave uses), then starts it via
`docker-compose` on `:8444` with a health check. Docker is a local-dev
convenience; the production artifact is the Windows `.exe`.

## Tests

We follow a shared set of testing conventions (adopted from the sibling `weave`
project):

- **Doc block** — every `_test.go` opens with a `/* */` block scoped to its
  production file, listing what's tested and what isn't:

  ```go
  /*
  Testing: config.go

  Pending:            // functions with no coverage yet
  Tested:             // prodFunc -> - TestFunc: description
  Tested elsewhere:   // covered in integration/E2E or another package
  Declined:           // deliberately not tested, with reason
  Additional Remarks: // free-form; always present
  */
  ```

  All five sections are always present, even when empty.

- **Library:** [`testify`](https://github.com/stretchr/testify) — `require` for
  preconditions (stops the test), `assert` for verifications (continues).
- **Naming:** `TestUnit_ShouldBehaviorWhenCondition` (the `When` clause is
  optional). Table-test cases use the same "should … when …" phrasing.
- **Structure:** Arrange-Act-Assert.
- **One `_test.go` per `.go` file**, even when there are no tests yet (the doc
  block records what's still pending). Prefer table-driven tests; every subtest
  calls `t.Parallel()`.

Run them with `task test` (race detector + shuffled order).

## CI

Every push and pull request runs the quality gate: build (host + `windows/amd64`),
`go vet`, tests with the race detector, `golangci-lint`, a generated-artifacts
check, and `govulncheck`. Pushing a `v*` tag additionally runs the release
workflow. Pushes that change only `docs/` skip CI (nothing to gate).
