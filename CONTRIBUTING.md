# Contributing to cxthub

Thank you for helping make coding-agent context portable, inspectable, and
safe to share.

## Start with the problem

Before opening a change:

- Search existing issues, discussions, and pull requests.
- Use GitHub Discussions for questions and early design exploration.
- Open an issue before non-trivial features, protocol or schema changes,
  persistence changes, migrations, compatibility changes, or broad refactors.
- Report vulnerabilities only through the private process in
  [SECURITY.md](SECURITY.md).

Small fixes, tests, and documentation improvements may go directly to a pull
request. Maintainers may close speculative features, unrequested rewrites, or
pure refactors that do not demonstrate a user or maintenance benefit.

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

PostgreSQL adapter or migration changes should run `make test-postgres` against
PostgreSQL 16. Web changes should run `npm ci` and `npm run build` from
`frontend/web`.

## Branches, commits, and pull requests

Follow [docs/BRANCHING.md](docs/BRANCHING.md):

- Branch from current `main` using a documented short-lived prefix.
- Keep one logical change per pull request.
- Use Conventional Commit form for commits and the pull-request title.
- Link the issue that the pull request resolves.
- Explain the user impact, not only the implementation.
- Add regression tests for defects and focused tests for new behavior.
- Include commands, logs, or screenshots that demonstrate verification.
- Do not edit release notes for an unreleased version unless a maintainer asks.

Pull requests are squash-merged after required checks and review. New pushes
dismiss stale approvals.

## Engineering invariants

- Snapshot identity is content-addressed.
- Natural snapshot parents are immutable.
- Graph algorithms must account for both natural and active overlay parents.
- CLI and backend Go modules remain independently buildable.
- Shared wire and persistence contracts live under `schemas/`.
- Provider-specific Claude Code and Codex formats stay at codec, capture, and
  materialization boundaries.
- Cloning the repository must never activate local coding-agent hooks.
- Never add real agent sessions, `.cxt`, `cxt-data`, credentials, local
  databases, production inputs, or private repository data to tests or
  fixtures.
- User-facing web copy must keep Korean and English locale keys aligned.

Changes that affect identity, graph reachability, persistence, synchronization,
provider conversion, authentication, authorization, or migrations require an
explicit compatibility and data-integrity explanation in the pull request.

## AI-assisted contributions

AI-assisted contributions are welcome, but the author remains responsible for
every submitted line.

Disclose material AI assistance in the pull request. Review generated code,
tests, licenses, and citations; remove private context; and verify the change
locally. Do not submit raw agent output or large generated rewrites without
human validation.

## Privacy and test data

Use synthetic, minimal fixtures. Never submit credentials, private source code,
customer data, personal transcripts, or unredacted session records. If a
credential may have been exposed, revoke it before continuing.

## License

By submitting a contribution, you certify that you have the right to submit it
and agree that it is licensed under the Apache License 2.0. No contributor
license agreement is currently required.

All contributors must follow [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
