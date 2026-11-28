# Changelog

All notable changes to CXTHub are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Removed

- The server reads GitHub only through the GitHub App: a repository that no
  installation was told to read is not read, also where its owner has no
  connection. The operator token `CXT_GITHUB_TOKEN` is no longer read; a server
  without a GitHub App reads public repositories without a credential.
- The repository webhook `/api/v1/hooks/github` (`CXT_GITHUB_WEBHOOK_SECRET`,
  `scripts/github-webhook.sh`, `make webhook-check` and `make webhook-apply`)
  is removed: GitHub tells the server through the App's webhook. Delete such a
  webhook from the GitHub repository's settings.

### Upgrade notes

- Terraform no longer has the input `github_webhook_secret_id` or the outputs
  `github_webhook_secret` and `github_webhook_url`: remove the input from a
  tfvars file that sets it. Terraform never managed the secret
  `cxt-github-webhook-secret`; delete it from Secret Manager after the apply.
- `scripts/dogfood-daemon.sh restart` drops a `CXT_GITHUB_TOKEN` that an
  earlier version stored in the LaunchAgent; revoke that token at GitHub.
- On a server with a GitHub App, a repository that no installation was told
  to read is not read until a maintainer connects it; connecting it starts
  its first read.

## [0.2.0] - 2026-11-28

Sessions are no longer captured by hooks at the moment something happens.
`cxt` reads what the coding agents and Git already record, uploads it, and the
server reads the Git host. The capture approach this replaces is removed. See
the upgrade notes before deploying.

### Added

- Sessions read from the records of Claude Code and Codex and linked to the
  commits they made: `cxt segments`, with `push`, `log`, `doctor`, `fork`,
  `remove`, `allow`, `archive`, `unarchive`, `refresh` and `resend`, each also
  a top-level command. A collector uploads them without being asked; hooks
  only wake it.
- A Sessions tab: sessions by commit, branch and worktree, their conversation,
  a summary at each commit, search, notes, archive, comparison of two commits
  and fork from a commit.
- The server reads branches, commits and pull requests from GitHub when it is
  told of a push and otherwise every six hours. A session opens, part by part,
  to whoever may view the repository as what it explains is pushed.
- A repository says whether it is connected to GitHub through the GitHub App:
  in the Sessions tab, in `cxt setup` and in `cxt doctor`.
- A long session's conversation is kept in several documents, so a session
  that goes on converts and sends only what was added.
- `cxt-admin repo-address` moves a stored repository, in the database, to the
  address its workspace is reached at now. `cxt setup` with that address then
  moves the origin of a clone that reaches the server through a transport
  endpoint.
- The tool server reads sessions through tools of their own.
- A copy of what was uploaded for each session, kept under `.cxt/sessions`.
- PostgreSQL migrations 0161 to 0168 and 0170 for sessions, branch name
  lifetimes, notes, what the Git host holds, what is open to readers,
  summaries, archive, removal and collector reports.
- Additive PostgreSQL migration 0093 for verified conversation range indexes;
  existing archives and document identities remain unchanged.

### Changed

- `cxt push`, `cxt log`, `cxt fork` and `cxt doctor` now mean the session
  commands.
- Agent and Git hooks capture nothing; they wake the collector.
- A repository opens on the Sessions tab.
- The Git remote cxt follows need not be named `origin`; `cxt setup` chooses
  it.
- The server's document worker is woken when a document is stored, so an
  upload no longer waits up to a second for it.

### Removed

- Capturing: `save`, `add`, `commit`, `restore`, `status`, `diff`,
  `checkout`, `pull`, `stash` and the other capture commands, and the
  wrappers `cxt claude` and `cxt codex`.
- The Context and On Hold tabs with the commit graph of captures, the
  document reader, the memory panel and PR promotions; in settings, the
  default branch, its protection and history protection; on profiles, the
  contribution calendar and the activity list.
- The routes and tool-server tools that took or served captures.
- The tables of the capture approach that nothing depends on (migration 0169).

### Fixed

- Read conversation pages from verified range indexes, load tool details on
  demand, and share bounded resources across reads, uploads and workers.
- Keep global Codex and Claude lifecycle hooks inert in Git repositories that
  have not run `cxt init` or `cxt setup`.
- Preserve and quarantine directory-only `.cxt` residue from older hooks while
  automatically registering `.cxt/` and `.cxtsecrets` in Git ignore files.
- `cxt setup` says whether automatic upload is on in the repository, which it
  is by default, and how to turn it off or on. Sessions that ended before it
  began go with `cxt push`.
- The server logs the cause of every error it answers with "internal server
  error", with the method, path and `X-Request-ID` of the request. Before, an
  operator could not see why a request failed.

### Upgrade notes

- Sessions recorded before the collector first ran on a computer are uploaded
  only by `cxt push`. Run it once on every computer whose sessions should be
  on the server.
- Migration 0169 drops data and cannot be undone from the database. Take a
  backup first. A server of an earlier version does not start on a migrated
  database.
- Migrations 0161 to 0170 create 17 tables that the database accounts of the
  API, MCP and worker may not use until they are given rights on them. After
  `migrate --apply`, run `schemas/db/runtime_rights/0161_0170.sql` as the
  migration account; see "Rights on the tables of this release" in
  `docs/DATABASE_MIGRATIONS.md`. Without it the Sessions tab and every upload
  answer "internal server error".
- Update the CLI and the web before the server: an earlier CLI or web page
  fails against a server without the capture routes.
- With a GitHub App configured on the server, `cxt setup` ends as not complete
  for a repository whose owner has not connected it.

## [0.1.1] - 2026-07-29

### Changed

- Expanded the installation guide and CLI reference with release, update,
  removal, CI, restore-mode, synchronization, and configuration workflows.

### Fixed

- Return a normal empty result when optional team settings or shared secrets
  have not been configured.
- Capture current Codex rollouts that contain tool-search control records or
  object-form function arguments.
- Rebind restored Claude Code and Codex sessions to the destination working
  tree so relocated clones remain discoverable by live capture.
- Keep the symbolic context `HEAD` aligned with branch checkouts while leaving
  tag and direct-snapshot restores detached.
- Preserve the latest user request and valid tool-call structure when a large
  restored session must be reduced to the seed context budget.

## [0.1.0] - 2026-07-26

### Added

- Initial public alpha of the `cxt` CLI, `cxtd` server, and web application.
- Git-integrated capture, branch, restore, stash, push, and pull workflows.
- Claude Code and Codex capture, materialization, and cross-provider support.
- Content-addressed snapshots, chunked document transport, reflog, and
  integrity checks.
- Filesystem and PostgreSQL server stores.
- Workspace access control, pending-session review, shared settings, and
  end-to-end encrypted secret-mask distribution.

[Unreleased]: https://github.com/wnsdy95/cxthub/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/wnsdy95/cxthub/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/wnsdy95/cxthub/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wnsdy95/cxthub/releases/tag/v0.1.0
