# cxthub

[![CI](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml/badge.svg)](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/wnsdy95/cxthub?display_name=tag&sort=semver)](https://github.com/wnsdy95/cxthub/releases)
[![License](https://img.shields.io/github/license/wnsdy95/cxthub)](LICENSE)

cxthub is Git-style version control for coding-agent context. It captures
Claude Code and Codex sessions, connects them to Git history, restores them
across branches or providers, and synchronizes them through a self-hostable
server.

> [!WARNING]
> cxthub is alpha software. Back up important data, review captured context
> before sharing it, and never rely on automatic secret scrubbing as your only
> security control.

## What it does

- Captures agent sessions automatically through Git and provider hooks.
- Stores normalized context as content-addressed snapshots.
- Keeps context aligned with commits, branches, rebases, resets, and stashes.
- Restores sessions in full, reconstructed, or distilled-memory form.
- Converts supported context between Claude Code and Codex.
- Synchronizes immutable objects and mutable refs through `cxtd`.
- Provides a web UI for history, comparison, review, and workspace management.
- Shares selected agent settings and end-to-end encrypted secret-mask lists.

Natural snapshot parents are immutable. Repairs and session placement use
versioned overlay edges, so synchronization never rewrites a snapshot's
content identity.

## Components

| Path | Purpose |
|---|---|
| `cli/` | Go `cxt` CLI, capture adapters, codecs, local object store, and MCP server |
| `backend/` | Go `cxtd` HTTP server with filesystem and PostgreSQL stores |
| `frontend/web/` | React and TypeScript web application |
| `schemas/` | OpenAPI, JSON Schema, and ordered PostgreSQL migrations |
| `integrations/` | Claude Code and Codex integration assets |
| `deploy/` | Optional self-hosting infrastructure |

## Install

Release binaries support macOS and Linux on arm64 and amd64:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

Pin a version with `CXT_VERSION=v0.1.0`.

To build from source, install Go 1.26+, Node.js 22+, npm, and Git 2.28+:

```bash
git clone https://github.com/wnsdy95/cxthub.git
cd cxthub
make build
```

The binaries are written to `bin/cxt` and `bin/cxtd`.

## Quick start

Run these commands inside an existing Git repository:

```bash
cxt init
cxt remote add origin https://<host>/<username>/<workspace>
cxt login
cxt remote -v
```

`cxt init` installs Git hooks by default. Once installed, normal Git actions
capture and move context:

| Git action | cxt behavior |
|---|---|
| `git commit` | Capture staged active agent sessions and link the Git commit |
| `git switch` / `git checkout` | Restore the destination branch context |
| `git branch` | Create the corresponding context branch |
| `git rebase` / `git commit --amend` | Track rewritten Git commit links |
| `git stash` / `git stash pop` | Save or restore the active session |
| `git push` / `git pull` | Synchronize context with the configured origin |

Useful manual commands:

```bash
cxt save -m "investigated the failing sync path"
cxt list
cxt load <snapshot>
cxt push
cxt fsck
cxt --help
```

Use `cxt init --no-hooks` to opt out, or `cxt hooks uninstall` to remove the
managed hooks.

## Provider integrations

- `integrations/claude-code/` contains Claude Code plugin, command, MCP, and
  hook assets.
- `integrations/codex/` contains Codex MCP, prompt, and hook configuration.

The CLI can also wrap either provider directly:

```bash
cxt claude
cxt codex
```

## Secret handling

Place exact secret values, one per line, in `.cxtsecrets`. The CLI masks those
values before writing a capture. The file is ignored by Git and is not uploaded
in plaintext.

Pattern-based scrubbing and `.cxtsecrets` are defense in depth. If a credential
may have entered an agent session or Git history, revoke it immediately.

## Self-hosting

For local development:

```bash
bin/cxtd serve --addr 127.0.0.1:8907 --data ./cxt-data
```

The filesystem store and development authentication mode are for trusted local
use. Internet-facing installations must configure TLS, production
authentication, PostgreSQL, backups, rate limits, and secret management.

The REST API contract is [schemas/openapi.yaml](schemas/openapi.yaml).

## Development

```bash
make build
make test
make typecheck
make lint
make e2e
make e2e-sync
make public-check
```

PostgreSQL changes should also run `make test-postgres` against PostgreSQL 16.
See [CONTRIBUTING.md](CONTRIBUTING.md) before opening an issue or pull request.

## Project policy

- [Contributing](CONTRIBUTING.md)
- [Governance](GOVERNANCE.md)
- [Security](SECURITY.md)
- [Support](SUPPORT.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)
- [Branching](docs/BRANCHING.md)
- [Releasing](docs/RELEASING.md)

## License

Licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE),
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md), and
[TRADEMARKS.md](TRADEMARKS.md).
