# Branching and integration

CXTHub follows a trunk-based GitHub Flow. `main` is the only long-lived
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

When the project has only one eligible maintainer and that maintainer authors a
pull request, GitHub cannot accept the author's own approval. In that
solo-maintainer state, the owner may use the ruleset bypass after every required
status check passes and must record the reason in the pull request. The bypass
replaces only the unavailable independent approval: the change must still use a
short-lived branch, signed commits, a pull request targeting `main`, resolved
review conversations, and linear history.

The solo-maintainer exception never permits direct pushes to `main`, merging
failed checks, force-pushing protected refs, or moving release tags. It ends
when another eligible maintainer or code owner is available. Outside this
narrow exception, the owner bypass remains reserved for emergency security
work, release recovery, and authorized history maintenance. When
confidentiality does not prevent it, every bypassed change must retain a public
audit trail.

## Releases and fixes

Releases are immutable tags cut from `main`; there are no release branches.
Urgent fixes branch from `main`, follow the normal review flow when possible,
and produce a new patch release. Never move or replace a published release tag.

See [RELEASING.md](RELEASING.md) for the release procedure.
