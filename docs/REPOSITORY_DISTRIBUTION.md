# Generated repository distribution

## Ownership and boundaries

The public [`wnsdy95/cxthub`](https://github.com/wnsdy95/cxthub) monorepo remains
the single source of truth. Development, reviews, integration tests, schema
changes, and release decisions stay here under [CONTRIBUTING](../CONTRIBUTING.md)
and [BRANCHING](BRANCHING.md). Component repositories are generated deployment
or distribution artifacts, not independent development trunks. Never edit a
mirror, merge a mirror-only fix, or synchronize changes back into the monorepo.

| Component | Destination | Visibility | Export layout |
|---|---|---|---|
| CLI | `wnsdy95/cxthub-cli` | Public | `cli/` flattened to the root; module `github.com/wnsdy95/cxthub-cli`; binary `cxt` |
| API backend | `wnsdy95/cxthub-backend` | Private | Complete `backend/` and `schemas/` at their original paths; `deploy/Dockerfile` mapped to root `Dockerfile` |
| Web frontend | `wnsdy95/cxthub-frontend` | Private | `frontend/web/` flattened to the root, including unchanged `vercel.mjs` |
| MCP service | `wnsdy95/cxthub-mcp` | Private | Complete `backend/` and `schemas/` at their original paths; `deploy/Dockerfile.mcp` mapped to root `Dockerfile` |

Private mirrors are an operational access boundary, not a claim that their
source is secret: the originating monorepo is public. The API and MCP mirrors
intentionally share the full backend module, whose existing module path stays
`github.com/wnsdy95/cxthub/backend`. They are different executable/container
distributions, not a new Go package split.

The CLI additionally includes `docs/CLI.md`, `integrations/**` including tracked
dotfiles, `schemas/cir.schema.json`, `schemas/testdata/**`, and
`scripts/test-go-batches.py` at those root-relative paths. Help and integration
tests require the CLI reference and integration assets; preserve their paths
and dotfiles rather than copying only visible directory entries. Shared
`LICENSE`, `NOTICE`, and third-party notices come from the source; do not
maintain mirror copies by hand.
`distrib/repository-templates/{cli,backend,frontend,mcp}/` supplies
each generated README and workflow. CLI release templates live there too. The
source allowlist in `distrib/repositories.json` also copies the source root
`.gitignore`; it is not a separately maintained component configuration.

PostgreSQL is the existing shared **resource**, not another source repository.
Both services use the same database and migrations. There is no fifth export,
database provisioning, data migration, or cloud resource creation in repository
distribution.

## Exact-commit export contract

- Resolve one full source commit SHA, and use it for every component in a
  release set. Read only the explicit tracked-file allowlist from that commit,
  including the templates; never export a dirty worktree or a moving branch.
- Copy snapshots, not source Git history. Do not use a fork, subtree history,
  or mirror push of the source repository. Initial component heads start new
  histories; later generated commits may retain their own export history.
- Record the source SHA and the component identity in `.cxthub-source.json`.
  It also records the source tree and exported file modes and SHA-256 hashes.
  Retain a release-set record mapping that SHA to all four destination commit
  SHAs and the verified exported trees. A component commit SHA is not the
  source commit SHA and need not match another component SHA.
- Fail closed on unexpected paths, credentials, missing inputs, or independent
  mirror edits. Do not overwrite drift or force-push to make a check pass.
- Exporting, pushing, or passing component CI must never call a cloud deploy
  hook or API. Publication and deployment are separate, manually approved
  operations.

The source monorepo maintains `distrib/repositories.json`,
`scripts/distribute-repositories.py`, provenance/release-set verification, and
the gated synchronization workflow. This runbook describes the maintainer
handoff, not a claim that publishing or cloud setup has already happened.
Do not substitute a broad recursive copy or an ad hoc push script.

## Checks before initial publication

1. Complete and review the source changes, including export tests and generated
   layouts. Follow the ordinary short-lived source branch and PR process.
2. Run the monorepo's required checks and applicable development tests, including
   CLI tests, backend tests, cross-module parity, PostgreSQL contracts, frontend
   unit/build checks, and full-stack E2E against a disposable database. Consult
   [CONTRIBUTING](../CONTRIBUTING.md) and source CI for the complete commands.
3. Merge the source PR only after those checks pass. Export validation before
   merge is a rehearsal, not permission to publish initial component heads.
4. Select the exact current source `main` commit, verify its latest push CI
   succeeded, and generate all four exports from that single SHA. Confirm the
   source commit contains the reviewed exporter/configuration and templates.
5. Check the generated repository layouts and run their packaging checks before
   authorizing the first publication. Do not treat an unmerged feature-branch
   SHA or successful partial component tests as the release gate.

### Generated CI is deliberately narrower

| Mirror | Checks | Limitation |
|---|---|---|
| CLI | Build, vet, ordinary package tests, bounded delivery/CLI test batches | No sibling backend; cross-module parity and integration stay in the monorepo |
| Backend | Default and `postgres` builds/vet, `cxtd` build, root Docker build | Full backend/cross-module/PostgreSQL test execution stays in the monorepo |
| Frontend | `test:unit`, architecture, locale, Vercel checks, typecheck/build | Go-backed graph contract suites and full-stack Playwright remain mandatory in monorepo CI |
| MCP | Default and `postgres` builds/vet, `cxt-mcp` build, root Docker build | Full backend/cross-module/OAuth/PostgreSQL test execution stays in the monorepo |

Mirror CI validates distribution packaging; it does not replace source CI.
Backend and MCP keep test files but do not pretend their complete test suites
can run without monorepo-only peers. Their READMEs explicitly explain this.

## Authentication and owner bootstrap

The dedicated repository-sync GitHub App **does not exist yet**. The user will
register it later. Do not repurpose the product's GitHub integration App or copy
an owner's PAT into Actions to bypass this pending setup.

For persistent synchronization, the owner must:

1. Register a dedicated GitHub App and install it on **only these four target
   repositories**, not every repository owned by the account.
2. Request repository **Contents: read and write** and **Workflows: write**.
   Workflows write is needed because the generated payload contains
   `.github/workflows/**`; it is not permission to administer Actions, secrets,
   or infrastructure. Metadata read access is implicit. Do not request unrelated
   repository, organization, or account permissions. GitHub documents the
   [Contents/Workflows permissions for Git refs](https://docs.github.com/en/rest/git/refs#create-a-reference).
3. Store `REPOSITORY_SYNC_APP_ID` as an Actions **variable** and
   `REPOSITORY_SYNC_APP_PRIVATE_KEY` as an Actions **secret**, in the **source
   monorepo only**. Never place them in the four mirrors, source files, export
   artifacts, command-line arguments, logs, or plaintext handoff notes.
4. Set source Actions variable `REPOSITORY_SYNC_ENABLED=true` only after review;
   the source workflow is disabled by default without that opt-in.
   It must obtain
   a short-lived installation token restricted to the four repositories and
   the necessary permissions, and use source `GITHUB_TOKEN` only for source
   access. Do not grant the sync App source write access. Keep the gate closed
   until the App and required source settings are present. Publishing needs no
   target Actions permission. The separate `verify` command reads target CI and
   therefore needs Actions read access if authenticated as an App. Keep this
   dedicated sync App's requested permissions unchanged: run final verification
   using the authorized owner's existing `gh` authentication instead. Do not
   silently add Actions read or a copied PAT to the persistent sync setup.
5. Configure target rulesets so only the approved generated-publication path
   can update `main`, while preventing deletion/force-push and protecting
   immutable release tags. Do not weaken the source PR rules for mirror sync.

An explicitly authorized owner bootstrap using existing owner authentication is
allowed before App registration, after source tests and merge. Use the same
exact-commit exporter and verification; never copy the owner's credential into
workflow secrets, a URL, or an export. Do not print token/key contents. Bootstrap
does not establish unattended sync and does not authorize deployment. Empty
repository creation or ownership changes require a separate owner action. The
owner has now created all four repositories empty with the visibility listed
above, with `main` as their default branch. No cloud integration is connected.
Initial heads and persistent App registration are still pending.

## Exporter commands

Run these from the source monorepo. Use a full, reviewed source SHA in
`SOURCE_SHA`. Local `export` is a read-only preparation step and may validate a
committed candidate before merge; **publishing initial heads still requires
the source PR to be merged and current source `main` push CI to pass**.

```bash
python3 scripts/distribute-repositories.py export \
  --ref "${SOURCE_SHA:?set the full source commit SHA}" \
  --component cli --output "../repository-exports/$SOURCE_SHA/cli"
```

Repeat with `--component backend`, `frontend`, and `mcp` and a matching output
path. Each output directory must be **new**. Export reads the selected commit,
not uncommitted edits. Check `.cxthub-source.json` in each output.

Before publication, install the required local tools, including `gitleaks` on
`PATH`, and use existing authorized owner `gh` authentication or the gated
App workflow. HTTPS Git credentials are supplied through `gh auth git-credential`:
the App workflow uses its short-lived `GH_TOKEN`; owner bootstrap falls back
to local `gh` authentication. Never copy a token into a remote URL or this
documentation. The workflow handles credentials; do not print or paste them.

After the source merge/check gate and explicit publication authorization:

```bash
python3 scripts/distribute-repositories.py publish \
  --ref "${SOURCE_SHA:?set the current verified source main SHA}" \
  > ../repository-publication-receipt.json
```

`publish` checks current source `main` and its latest successful push CI,
destination visibility, provenance, drift, and stale heads. It scans exports
with `gitleaks`, refuses non-fast-forward updates, and emits a JSON publication receipt on
stdout. This receipt is **not deployment-ready**; target CI is still a separate
gate. Only retain a receipt as successful if the command exits successfully.

The publisher constructs each commit with the exact observed target head as
its parent, checks ancestry, then uses an explicit expected-head Git lease.
The `--force-with-lease=refs/heads/main:<expected>` flag serves only as a
compare-and-swap fence: it cannot authorize rewriting history in this path.
An intervening forward edit, reset, deletion, or concurrent initial creation
fails instead of being overwritten. Source eligibility is checked again before
each write and before certification. Four repositories still do not form one
cross-repository transaction; a superseded source can leave an incomplete set.

CI completion events are wakeups, not publication revisions. The gated workflow
always reads protected `main` after obtaining its execution slot and requires
successful CI for that exact current revision. Thus a delayed old completion
cannot replace a queued new publication with a permanently stale revision.

After all four target push CI runs finish, the authorized owner verifies with
local `gh` authentication that can read all four repositories' Actions results.
Do not run this step under the limited sync App token:

```bash
env -u GH_TOKEN -u GITHUB_TOKEN \
  python3 scripts/distribute-repositories.py verify \
  --ref "${SOURCE_SHA:?set the published source commit SHA}" \
  > ../repository-deployment-ready-receipt.json
```

`verify` requires all four remote `main` exports to match exactly and their
latest **push CI** runs to succeed. It emits the deployment-ready JSON receipt
containing immutable component commit SHAs. These are pinned commit identities,
not a claim that the local receipt file is tamper-proof; retain it in the
operator's controlled release record. Neither command deploys cloud services.

## Publish and verify one release set

The four pushes are **not atomic**. Even a successful source workflow step or
three successful pushes is not a completed release set.

1. Before the first push, confirm automatic Render deployments and Blueprint
   synchronization are disabled and Vercel Git deployment is disconnected.
   Do not connect new deployment integrations during bootstrap.
2. Publish the four generated heads using the approved owner bootstrap or the
   gated source App workflow. Record each destination's actual commit SHA.
3. After **all four pushes**, fetch each remote `main` and verify its tree and
   provenance against the export for the same selected source SHA. Use the
   exporter's release-set verification, not commit messages alone. Confirm the
   repository identity, public/private visibility, expected files, and absence
   of source history or extra payloads.
4. Wait for all four component CI results on those exact exported commits and
   retain the source CI evidence and complete release-set verification record.
   No result is not a success. Verification requires the push-triggered CI for
   every target; a manually dispatched run does not substitute for it. Restore
   the correct push trigger or resolve the failed run without editing a mirror.
5. Only then hand the verified source SHA, four destination SHAs, check results,
   and rollback reference to the person responsible for manual deployment.
   Re-check remote heads immediately before handoff; use the recorded exact
   commits rather than resolving a later moving `main` at deploy time.

If any push, verification, or check fails, mark the entire set incomplete and
deploy nothing. Keep the existing live services unchanged. Investigate drift,
retry only through the exporter using the same source SHA where appropriate,
then repeat verification of **all four** heads. If a new source fix is required,
merge it normally and publish a new complete set. Never repair a mirror by hand
or roll its branch back with a force-push. Retain the previous verified set for
an explicitly reviewed runtime rollback, including schema compatibility.

## Manual deployment handoff

This section is an operator plan, not authorization to create resources or
change a service now. No cloud credentials or deploy hooks belong in generated
CI or repository-sync automation.

### Render API and MCP

[`deploy/render-repositories.yaml`](../deploy/render-repositories.yaml) is the
repository-based handoff alternative to the unchanged root `render.yaml`. It
preserves both service names, plans, region, health checks, ports, environment
values/references, and the complete database block. Only the repository/branch,
root Dockerfile path, and manual deployment policy differ.

Render's [Blueprint specification](https://render.com/docs/blueprint-spec)
supports explicit service `repo` URLs, root-relative `dockerfilePath` and
`dockerContext`, and `autoDeployTrigger: off`. The private repository connection
must have access to both backend and MCP targets. The quoted `"off"` value in
the YAML is intentional: it is a string, not a YAML 1.1 boolean.

- API source: `https://github.com/wnsdy95/cxthub-backend`, branch `main`,
  Dockerfile `./Dockerfile`, context `.`.
- MCP source: `https://github.com/wnsdy95/cxthub-mcp`, branch `main`,
  Dockerfile `./Dockerfile`, context `.`.
- Keep the existing `cxthub-postgres` PostgreSQL 16 resource and both DSN
  references unchanged. Preserve the existing Firebase project and public
  frontend/custom-domain origin, API secure-cookie setting, and exact CORS
  origin reference. Keep existing secrets in their service settings.

Service auto-deploy settings do **not** disable Blueprint automatic sync. Set
the existing Blueprint's **Auto Sync to No** separately. Do not import a second
Blueprint for the same resources or click a creation/deployment confirmation as
a validation step. The owner must review a single existing Blueprint's source
path transition and every proposed change; stop if it would create or replace
services or the database. See Render's
[Blueprint ownership and manual sync guidance](https://render.com/docs/infrastructure-as-code).

After the full release set passes verification and an operator approves the
handoff, manually deploy each recorded component SHA using Render's **Deploy a
specific commit**, not **Deploy latest commit**. Check both `/healthz` endpoints
and shared-database readiness before the frontend cutover. The deployment
operator must review migration/backward compatibility and the rollout order;
for the combined-API-to-MCP split, follow the specific ordering in
[deploy/RENDER.md](../deploy/RENDER.md#migrating-from-combined-cxtd).
See [Render's manual deployment documentation](https://render.com/docs/deploys#manual-deploys).

### Vercel frontend

Keep the existing project's Git connection disconnected for manual-only
operation; Vercel documents this under
[Git settings](https://vercel.com/docs/project-configuration/git-settings#disconnect-your-git-repository).
This avoids changing the exported root `vercel.mjs` just to suppress automatic
Git deployments. The mirror's CI has no deployment step.

For a later owner-approved deployment, use a clean checkout of the recorded
`cxthub-frontend` commit, the existing Vercel project, repository root `.`,
`npm run build`, and output `dist`. Preserve the root `vercel.mjs` byte-for-byte
from `frontend/web/vercel.mjs`, including rewrite order and security headers.
Use the existing environment settings from [deploy/RENDER.md](../deploy/RENDER.md):
distinct `CXT_API_ORIGIN` and `CXT_MCP_ORIGIN`, the three Firebase web settings,
and no `VITE_API_BASE` override for the same-origin setup. Never store service
secrets in browser-exposed `VITE_*` variables. The public frontend origin,
Firebase authorized domains, and API/MCP identity must stay consistent.

### CLI compatibility and future releases

The CLI mirror README starts with a source build, not a promise that downloads
exist. Existing `distrib/install`, its monorepo URLs, source `.goreleaser.yaml`,
and source release workflow are unchanged. Do not redirect old install paths
or migrate existing release tags as part of repository bootstrap.

The generated root `goreleaser.yaml` builds `./cmd/cxt` as `cxt`. Its release
workflow is manual, uses only that repository's `GITHUB_TOKEN`, validates a
`vX.Y.Z` tag reachable from exported `main`, reruns CLI checks, and refuses an
existing release. After release-set verification and green CI, a maintainer may
create a new immutable tag from exported `main` and dispatch that workflow from
`main` with the tag name. Tag protections are a prerequisite: the workflow is
not a substitute for rules preventing tag deletion or movement. Failed or
published releases must not be repaired by replacing a tag or overwriting
assets; use a new patch tag. No cloud deployment follows a CLI release.

## Handoff checklist

Prepared source artifacts and confirmed repository setup; not evidence of a
live rollout:

- [x] Document four repository boundaries and single-source ownership.
- [x] Add per-component README and CI templates with explicit test limitations.
- [x] Prepare root CLI release templates without changing legacy install paths.
- [x] Prepare manual-only Render repository mapping with the same DB/env contract.
- [x] Verify Render field semantics and manual-sync behavior against official docs.
- [x] Owner created all four repositories empty with the specified visibility
  and default branch `main`; no cloud integration is connected.

Standalone CLI build/vet and focused tests have passed during preparation.
Broader CLI test runs reached their existing ten-minute timeout; this is not a
full-suite pass. Full required CI remains pending and must pass before source
merge and initial publication.

Still required before initial publication or deployment:

- [x] Review the source exporter, allowlist, provenance verification, and gated
  synchronization workflow; validate a committed rehearsal export without pushing.
- [ ] Pass monorepo development/integration tests and required CI; merge the
  source PR before publishing any initial component head.
- [ ] Owner confirms deployment/verification access and target rulesets.
- [ ] User registers the dedicated sync App later, scopes its installation and
  permissions, and stores the two settings only in the source repository;
  enable `REPOSITORY_SYNC_ENABLED=true` only after this setup is reviewed.
- [ ] If using owner bootstrap first, record that persistent App sync is pending.
- [ ] Confirm Render Auto-Deploy Off, Blueprint Auto Sync No, and disconnected
  Vercel Git deployment before publishing any component heads.
- [ ] Publish and verify all four heads as one release set; collect green CI on
  each exact destination commit and retain the source CI evidence.
- [ ] Owner approves the manual service/repository handoff without creating or
  replacing any resource; verify health, routing, and rollback readiness.
- [ ] Separately approve any first CLI binary release and immutable tag.
