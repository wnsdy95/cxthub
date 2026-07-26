# Branching and integration

cxthub follows a trunk-based GitHub Flow. `main` is the only long-lived
development branch and should remain releasable.

## Branch names

Create short-lived branches from the latest `main`:

| Prefix | Use |
|---|---|
| `feat/` | User-visible capability |
| `fix/` | Defect or regression |
| `docs/` | Documentation-only change |
| `test/` | Test-only improvement |
| `refactor/` | Behavior-preserving code change |
| `chore/` | Tooling, dependencies, or repository maintenance |

Use a short lowercase description after the prefix, for example
`fix/chunk-pull-limit`. Include an issue number when it improves traceability.

Do not create permanent development, staging, contributor, or version branches.
Security work may use a temporary private fork or security-advisory branch.

## Change flow

1. Search open issues, discussions, and pull requests.
2. Open an issue before non-trivial features, protocol changes, persistence
   changes, migrations, compatibility changes, or broad refactors.
3. Branch from current `main`.
4. Keep the branch focused and rebase it when needed. Do not merge `main` into
   the branch merely to resolve drift.
5. Open a pull request early enough for design and test feedback.
6. Address review threads and keep required checks green.
7. A maintainer squash-merges the pull request.
8. Delete the source branch after merge.

Pull requests must target `main`. The repository does not accept direct pushes
to `main` from ordinary contributors.

## Commit and pull-request titles

Use Conventional Commit form:

```text
type(optional-scope): imperative summary
```

Accepted types are `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`,
`ci`, `chore`, and `revert`.

Examples:

```text
feat(cli): add bounded chunk negotiation
fix(codec): preserve native image blocks
docs: explain release candidates
```

Mark incompatible changes with `!` and explain the migration in the body:

```text
feat(api)!: require scoped repository tokens
```

Individual branch commits may be iterative, but the pull-request title must be
release-quality because it becomes the squash commit on `main`.

## Protected branch policy

Repository rules require:

- pull requests for changes to `main`;
- one approving review, including code-owner review where applicable;
- dismissal of stale approvals after new pushes;
- resolution of review conversations;
- all required CI checks;
- signed commits;
- linear history;
- no branch deletion or force-push.

The repository owner has an explicit ruleset bypass for emergency security
work, release recovery, and authorized history maintenance. Bypass is not the
normal merge path. When confidentiality does not prevent it, bypassed changes
must retain an issue, pull request, release note, or other public audit trail.

## Releases and fixes

Releases are immutable tags cut from `main`; there are no release branches.
Urgent fixes branch from `main`, follow the normal review flow when possible,
and produce a new patch release. Never move or replace a published release tag.

See [RELEASING.md](RELEASING.md) for the release procedure.
