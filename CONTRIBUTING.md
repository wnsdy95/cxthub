# Contributing to cxthub

Thanks for helping make coding-agent context portable and inspectable.

## Before opening a change

- Use GitHub Discussions for design questions and feature exploration.
- Search existing issues before filing a bug.
- Report vulnerabilities through the private process in `SECURITY.md`.
- Keep pull requests focused; large protocol or data-model changes should have
  an issue or design discussion first.

## Development setup

Prerequisites are Go 1.26+, Node.js 22+, npm, and Git 2.28+.

```bash
git clone https://github.com/wnsdy95/cxthub.git
cd cxthub
make build
make test
make typecheck
```

Useful checks:

```bash
make lint
make e2e
make e2e-sync
make public-check
```

PostgreSQL adapter changes should also run `make test-postgres` with a real
PostgreSQL 16 test database when possible. Web changes should run `npm ci` and
`npm run build` in `frontend/web`.

## Architecture rules

- Preserve content-addressed snapshot identity and immutable natural parents.
- Treat `Parents ∪ GraftParents` as the reachability graph wherever graph
  reachability is calculated.
- Keep CLI and backend Go modules independent; language-neutral contracts live
  under `schemas/`.
- Provider-specific Claude Code and Codex data belongs at codec/materializer
  boundaries, not in shared domain behavior.
- Never add real agent sessions, `.cxt`, `cxt-data`, root-level agent settings,
  credentials, local databases, or production Terraform inputs to fixtures.
- Keep provider integrations under `integrations/`; cloning the repository
  must not activate `.claude`, `.codex`, or other local agent hooks.
- User-facing web copy must update both Korean and English locale files.

Read `docs/ARCHITECTURE.md`, `docs/DATA-MODEL.md`, and
`docs/SYNC-PROTOCOL.md` before changing persistence or synchronization.

## Pull requests

Explain the user-visible behavior, important invariants, and verification you
performed. Add regression tests for bug fixes. CI must pass before merge.
Maintainers may ask that unrelated changes be split into separate pull
requests.

By submitting a contribution, you certify that you have the right to submit it
and agree that it is licensed under Apache License 2.0. No contributor license
agreement is currently required.
