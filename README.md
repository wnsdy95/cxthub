<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="frontend/web/public/cxt-logo-dark-theme.svg">
    <source media="(prefers-color-scheme: light)" srcset="frontend/web/public/cxt-logo-light-theme.svg">
    <img src="frontend/web/public/cxt-logo-light-theme.svg" alt="" width="38" align="absmiddle">
  </picture>
  CXTHub
</h1>

**Where the code goes, the context follows.**

Git-native shared memory for coding agents.

[![CI](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml/badge.svg)](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/wnsdy95/cxthub?display_name=tag&sort=semver)](https://github.com/wnsdy95/cxthub/releases) [![License](https://img.shields.io/github/license/wnsdy95/cxthub)](LICENSE)

> [!WARNING]
> CXTHub is alpha software. Back up important data, review captured context
> before sharing it, and never rely on automatic secret scrubbing as your only
> security control.

## The repository remembers the change. It should remember the reason.

Git preserves outcomes with extraordinary discipline: commits, diffs,
branches, authors, and dates. The investigation and judgment that made those
outcomes necessary still disappear inside temporary agent conversations.

CXTHub captures and versions the missing context alongside the code. A
developer or agent can return to a branch, hand work to a teammate, or continue
in another supported agent without reconstructing the reasoning from scratch.

It is not a chat archive, a project wiki, or a second workflow beside Git. It
is a context layer that follows the lifecycle of the code.

## No new workflow

CXTHub follows Git actions developers already use:

| Git action | CXTHub behavior |
|---|---|
| `git commit` | Capture staged active agent sessions and link them to the commit |
| `git switch` / `git checkout` | Restore the destination branch context |
| `git branch` | Create the corresponding context branch |
| `git rebase` / `git commit --amend` | Track rewritten commit links |
| `git stash` / `git stash pop` | Preserve or restore unfinished agent work |
| `git push` / `git pull` | Synchronize context with the configured remote |

Install the hooks once with `cxt init`. After that, the code and the context
move together.

## What CXTHub carries forward

- **Sessions** — capture Claude Code and Codex work through provider and Git
  hooks.
- **Snapshots** — store normalized context as immutable, content-addressed
  states.
- **Commit links** — connect the session state to the code change it shaped.
- **Branches and stashes** — reorient or resume when repository work moves.
- **Agent transfer** — materialize supported context for Claude Code or Codex,
  regardless of which one created it.
- **Team history** — push and pull shared context through a self-hostable
  `cxtd` remote.
- **Retrieval** — let compatible agents inspect repository context through the
  read-only MCP server.
- **Review** — inspect history, comparisons, pending sessions, and workspace
  state in the web interface.

The underlying model is deliberately Git-like:

```text
agent session → cxt snapshot → Git commit → cxtd remote → person or agent
```

Natural snapshot parents are immutable. Repairs and session placement use
versioned overlay edges, so synchronization can preserve reachability without
rewriting a snapshot's content identity.

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

Useful manual commands:

```bash
cxt save -m "investigated the failing sync path"
cxt list
cxt load <snapshot>
cxt push
cxt fsck
cxt --help
```

Use `cxt init --no-hooks` to opt out of automatic Git integration, or
`cxt hooks uninstall` to remove the managed hooks.

## Provider integrations

- `integrations/claude-code/` contains the Claude Code plugin, command, MCP,
  and hook assets.
- `integrations/codex/` contains the Codex MCP, prompt, and hook
  configuration.

The CLI can also start either provider with repository context:

```bash
cxt claude
cxt codex
```

## Privacy is architecture

CXTHub processes coding-agent conversations, which may contain source code,
credentials, customer data, or internal decisions.

Place exact secret values, one per line, in `.cxtsecrets`. The CLI masks those
values before writing a capture. The file is ignored by Git and is not uploaded
in plaintext. Shared secret-mask lists are end-to-end encrypted.

Pattern-based scrubbing and `.cxtsecrets` are defense in depth. If a credential
may have entered an agent session or Git history, revoke it immediately. See
[SECURITY.md](SECURITY.md) for the disclosure process and security boundaries.

## Components

| Path | Purpose |
|---|---|
| `cli/` | Go `cxt` CLI, capture adapters, codecs, local object store, and MCP server |
| `backend/` | Go `cxtd` HTTP server with filesystem and PostgreSQL stores |
| `frontend/web/` | React and TypeScript web application |
| `schemas/` | OpenAPI, JSON Schema, and ordered PostgreSQL migrations |
| `integrations/` | Claude Code and Codex integration assets |
| `deploy/` | Optional self-hosting infrastructure |

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
