# Creating the public repository

> Status: preparation guide. Creating, deleting, publishing, or pushing a
> GitHub repository remains an explicitly approved manual operation.

The public cxthub repository must not be created by changing the visibility of
the existing private repository or importing its history. Export only the
validated current file set and start with **one new initial commit**. This
structurally excludes private dogfood commit messages, reflogs, deleted files,
and historical credentials from the public DAG and object database.

## 1. Repository identity

Use `github.com/wnsdy95/cxthub` for the public repository. Both Go modules and
all internal import paths use this location as their canonical identity. A
different owner or repository name requires a separate, reviewed module-path
migration before publication.

Do not use any of these methods:

- changing the existing private repository to public;
- GitHub Importer, a fork, or a template option that preserves history;
- `git clone --mirror`, `git push --mirror`, or bundle restoration;
- copying the existing `.git` directory.

## 2. Publication blockers

1. Revoke the GitHub PAT that may have been exposed during private dogfooding.
2. Revoke the old cross-repository `RELEASE_TOKEN` as well.
3. Set `CXT_CONFIRMED_LEAKED_PAT_REVOKED=true` only after an operator has
   directly confirmed revocation in GitHub. The variable does not revoke a
   credential by itself.
4. `make public-check` must pass for the exported tree.
5. After creating the initial commit, `make public-check-full` must pass,
   including the complete Gitleaks history scan.

## 3. Exporting a clean tree

Choose an unused path outside the source repository, such as a sibling
directory. The source worktree must be clean: every public file must first be
reviewed and committed, while every local or generated file must be ignored.

```bash
export CXT_CONFIRMED_LEAKED_PAT_REVOKED=true
scripts/export-public-tree.sh ../cxthub-public

cd ../cxthub-public
git status --short
git diff --cached --stat
git config user.name
git config user.email
git commit -S -m "Initial public release"
test "$(git rev-list --count --all)" -eq 1
make public-check-full
```

The export script uses `git archive HEAD`, so the new tree is exactly the
reviewed source commit. It never copies untracked files, ignored files, the old
`.git` directory, reflogs, remotes, deleted blobs, or private-history objects.
It fails if the source worktree is dirty, the destination already exists, or
the destination is inside the source repository.

### Public tree contract

The initial public commit includes:

- CLI, backend, web, and framework-independent frontend source;
- unit, integration, E2E, and migration tests;
- schemas, SQL migrations, public assets, package/module lockfiles, and
  reproducible build metadata;
- CI, release automation, self-hosting templates, public integration packages,
  licenses, governance files, and documentation.

It excludes:

- root-level agent configuration or state such as `.claude/`, `.codex/`,
  `.agents/`, `.mcp.json`, and editor-agent folders;
- `.cxt/`, `cxt-data/`, daemon state, local databases, logs, temporary files,
  coverage, profiles, caches, and build output;
- all real environment files, Terraform inputs/state, package-auth files,
  cloud credentials, private keys, and credential containers;
- the private repository's commits, tags, reflogs, remotes, author metadata,
  deleted files, and unreachable Git objects.

The committed integration examples under `integrations/` are the opt-in
distribution path. Cloning the source repository must never activate local
coding-agent hooks automatically.

The one-commit assertion is a one-time publication check. After publication,
`make public-check-full` continues scanning the complete growing history and
does not restrict normal future commits.

## 4. GitHub repository settings

Create an empty repository without an auto-generated README or license, then
configure:

- visibility: public;
- default branch: `main`;
- private vulnerability reporting: enabled;
- secret scanning and push protection: enabled;
- Discussions: enabled;
- default Actions token permission: read-only;
- workflow access to secrets from fork pull requests: disabled;
- branch ruleset: required CI, no force pushes, no branch deletion, dismiss
  stale reviews;
- tag ruleset: no deletion or forced updates for `v*`;
- squash merge as the default merge method.

Only then add the remote and push the initial commit:

```bash
git remote add origin git@github.com:wnsdy95/cxthub.git
git push -u origin main
```

After the first push, verify that GitHub recognizes Apache-2.0, displays the
security policy and issue templates, and runs every required CI job on forked
pull requests without repository secrets.

## 5. Open source and hosted service boundary

- The source, `cxt`, `cxtd`, web UI, schemas, and self-hosting path are public
  under Apache-2.0.
- cxthub.com charges for managed infrastructure, backups, upgrades, and
  operations, not for permission to use the source.
- Community self-hosting support is best-effort and has no SLA.
- Official names and logos remain subject to `TRADEMARKS.md`.
- Tag releases use only the new repository's scoped `GITHUB_TOKEN`. Do not use
  a long-lived PAT to publish into another repository.

## 6. Post-publication verification

1. Clone onto a clean machine or into a new temporary directory.
2. Run `make build`, `make test`, `make typecheck`, and `make e2e`.
3. Run `docker build -f deploy/Dockerfile .`.
4. Run `make public-check-full`.
5. Use GitHub code search to confirm zero results for forbidden legacy
   identifiers and credential patterns.
6. Verify a GoReleaser snapshot and the installer before creating the first
   version tag.

The old private repository may remain access-restricted as a temporary backup
until the public repository is stable. It must never be used as the public
repository's remote or history source.
