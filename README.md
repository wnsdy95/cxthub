<h1>
  <img src="frontend/web/src/assets/cxt-logo-light-theme.png" alt="" width="38" align="absmiddle">
  CXTHub
</h1>

**Where the code goes, the context follows.**

Your coding-agent sessions, kept with the Git commits they produced.

[![CI](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml/badge.svg)](https://github.com/wnsdy95/cxthub/actions/workflows/ci.yml) [![Release](https://img.shields.io/github/v/release/wnsdy95/cxthub?display_name=tag&sort=semver)](https://github.com/wnsdy95/cxthub/releases) [![License](https://img.shields.io/github/license/wnsdy95/cxthub)](LICENSE)

> [!WARNING]
> CXTHub is alpha software. Back up important data, know who can read a
> repository before you connect it, and never rely on automatic secret
> scrubbing as your only security control.

## The repository remembers the change. It should remember the reason.

Git preserves outcomes with extraordinary discipline: commits, diffs,
branches, authors, and dates. The investigation and judgment that made those
outcomes necessary still disappear inside temporary agent conversations.

CXTHub keeps each coding-agent session with the commits it produced. The `cxt`
CLI reads what Claude Code and Codex recorded on your computer, and Git, cuts
each session at the commits it made, and uploads the result to your team's
server. Once a commit is pushed, the part of the session that explains it can
be read by whoever may view the repository: a teammate, or another agent that
continues the work.

It is not a chat archive, a project wiki, or a second workflow beside Git. It
is a layer that follows the lifecycle of the code.

## No new workflow

You keep using Git and your agent as before. `cxt setup` installs Git hooks
and agent hooks once. A hook only wakes the reading of the tools' own records
and of Git; it decides nothing and cannot stop a Git command. A hook that did
not run is made up for by the next one that does.

| You do | What follows |
|---|---|
| `git commit` | The session that made the commit is cut at it. That part is a segment linked to the commit, and a summary of the session is written at that commit |
| `git push` | Once the server sees the commit on GitHub, the segments that explain it open to whoever may view the repository |
| `git switch` / `git checkout` | Nothing is restored or replaced. At its start or next prompt, a session on the branch is given the most recent summary another session wrote along that branch |
| `git rebase` / `git cherry-pick` | The server recognizes the copy of a commit by its author time, author and subject, so the segment still opens when the copy is pushed |

## What CXTHub carries forward

- **Sessions** — read from the records Claude Code and Codex keep on your
  computer. No hook has to run at the right moment for a session to be kept.
- **Segments** — a session is cut at every commit it made. Each part is linked
  to its commit, and every link names the rule that made it.
- **Summaries** — at each commit a session made: what was done, what is worth
  keeping, and what is left to do. They are made by fixed rules, without
  asking a model.
- **Branch handoff** — a new session is given the most recent summary along
  the branch its folder is on.
- **Team reading** — a part of a session is read by you alone until the commit
  it explains is pushed. After that it is read by whoever may view the
  repository on your team's `cxtd` server, which you can host yourself.
- **Fork from a commit** — make a Git branch at a commit, or continue a
  session's conversation up to that commit as a new session.
- **Retrieval** — Codex, Claude, and compatible agents read sessions on the
  server through the OAuth-protected remote MCP server.
- **Review** — the web Sessions tab shows the commit graph with the segments
  at each commit, pull requests, branches, worktrees, notes, a search, and a
  comparison of two commits.

The underlying model follows Git:

```text
agent session + Git commit → segment → cxtd server → person or agent
```

## Install

Release binaries support macOS and Linux on arm64 and amd64:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install | sh
```

Pin a version by passing the variable to the installer shell:

```bash
curl -fsSL https://raw.githubusercontent.com/wnsdy95/cxthub/main/distrib/install |
  CXT_VERSION=v0.1.0 sh
```

To build the Go binaries from source, install Go 1.26.11+ and Git 2.28+:

```bash
git clone https://github.com/wnsdy95/cxthub.git
cd cxthub
make build
```

The binaries are written to `bin/cxt` and `bin/cxtd`.

See the [installation guide](docs/INSTALLATION.md) for manual archive
verification, custom install directories, upgrades, removal, source builds,
and the separate `cxtd` server.

The public [cxthub-cli](https://github.com/wnsdy95/cxthub-cli) repository is an
independently buildable, generated CLI distribution. This monorepo remains the
development source for all components. The existing installer and releases
remain supported; component synchronization does not deploy the service.
See [repository distribution](docs/REPOSITORY_DISTRIBUTION.md) for source
provenance, private deployment mirrors, and the deployment readiness gate.

## Quick start

Run these commands inside an existing Git repository:

```bash
cxt setup https://<host>/<owner>/<repository>
cxt doctor
```

`cxt setup` makes the repository's `.cxt` folder, logs in, verifies and
registers the repository connection, installs the Git hooks, merges the Claude
Code and Codex hooks into their settings, and pulls team settings. It is safe
to rerun: completed steps are reported and left as they are. Setup uploads
nothing itself. From then on the hooks wake a collector that uploads the
sessions recorded in this repository since it first ran. `cxt doctor` says
whether the collector runs and what, if anything, does not fit. See the
[setup reference](docs/CLI.md#cxt-setup) for each step.

Useful commands:

```bash
cxt segments   # show the sessions of this folder cut at their commits; reads only
cxt push       # upload the sessions now, including those recorded before setup
cxt log        # show what was uploaded from this computer
cxt doctor     # say what does not fit, with the command that puts it right
cxt --help
```

`cxt segments` needs no setup, server or login: it reads this computer alone.
Use `cxt config segments.auto off` to leave uploads to `cxt push`,
`cxt init --no-hooks` to set up without the Git hooks, or
`cxt hooks uninstall` to remove them.

The complete command and option reference is in
[docs/CLI.md](docs/CLI.md).

## Provider integrations

- `integrations/claude-code/` contains the Claude Code plugin, command, MCP,
  and hook assets.
- `integrations/codex/` contains the Codex MCP, prompt, and hook
  configuration.

`cxt setup` registers the agent hooks once: in the repository's
`.claude/settings.json` for Claude Code, and in `~/.codex/hooks.json` for
Codex. The same hooks cover Codex app and IDE sessions and Claude Desktop's
**Code** tab; no process wrapper is needed, and you start Claude Code and Codex
the way you normally do. Linked worktrees share the primary repository's
`.cxt` folder.

An agent hook does one of two things. At the end of a turn and of a session it
wakes the collector, which then reads the rest of a turn that committed or
pushed in its middle. At session start and at a prompt it gives the session
the most recent summary another session wrote along the branch its folder is
on, once for each branch the session comes to. That text is at most 16 KiB. A hook carries no conversation to the
server, and it never writes to the tool's own files.

Claude Desktop's general **Chat** tab is not a Claude Code session and does not
run these hooks. It is outside this integration.

## Privacy is architecture

CXTHub processes coding-agent conversations, which may contain source code,
credentials, customer data, or internal decisions.

Know what leaves your computer. In a connected repository the collector
uploads each session's conversation, after masking. On the server a session is
read by the account that uploaded it. Once a commit is pushed, the part of the
session that explains it is read by whoever may view the repository; in a
public repository that includes visitors who are not signed in. `cxt remove`
takes a session off the server for every reader.

Place exact secret values, one per line, in `.cxtsecrets`. The CLI masks those
values, and then applies the pattern scrub of the configured tier, before a
conversation is uploaded.
The file is ignored by Git and is not uploaded in plaintext. Shared
secret-mask lists are end-to-end encrypted.

Pattern-based scrubbing and `.cxtsecrets` are defense in depth. If a credential
may have entered an agent session or Git history, revoke it immediately. See
[SECURITY.md](SECURITY.md) for the disclosure process and security boundaries.

## Components

| Path | Purpose |
|---|---|
| `cli/` | Go `cxt` CLI: readers of the agent tools' session records and of Git, the session report, masking, codecs, and the upload |
| `backend/` | Independent Go `cxtd` API and `cxt-mcp` remote MCP servers; shared PostgreSQL in production |
| `frontend/web/` | Sole web runtime: React and TypeScript application, tests, and deployment config |
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

`make e2e` and `make e2e-sync` run the real `cxt` and `cxtd` binaries.
`make e2e-sync` runs the chain a Git hook starts: setup, a real commit hook,
the collector and the server. Both use the file store by default; set
`CXT_E2E_DSN` to a disposable PostgreSQL 16 database to run `make e2e-sync`
against PostgreSQL instead.

PostgreSQL changes should also run `make test-postgres` against PostgreSQL 16.
See [CONTRIBUTING.md](CONTRIBUTING.md) before opening an issue or pull request.

## Documentation

- [Installation](docs/INSTALLATION.md)
- [CLI reference](docs/CLI.md)
- [MCP connections](docs/MCP.md)
- [GitHub connections](docs/GITHUB_CONNECTIONS.md)
- [Architecture](docs/ARCHITECTURE.md)

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
