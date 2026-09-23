# Repository, Organization, Team, and Enterprise ownership

This document records the accepted replacement for the former
Namespace -> Workspace -> Repository product model. [Issue #251](https://github.com/wnsdy95/cxthub/issues/251) tracks delivery.

## Product contract

- A Repository corresponds to one code repository and owns context, memories,
  branches, visibility, collaborators, settings, and secrets.
- A User or Organization directly owns a Repository at `/{owner}/{repository}`.
- An Organization owns repositories, members, Teams, policies, and its audit log.
- A Team groups members of exactly one Organization and receives explicit roles
  on that Organization's repositories. Outside collaborators cannot join teams.
- An Enterprise groups Organizations and applies parent policy. It does not own
  repositories or implicitly grant access to private context.
- Pricing remains documentation-only; the migration must not activate billing.

The terminology follows the official GitHub documentation for
[organizations](https://docs.github.com/en/organizations/collaborating-with-groups-in-organizations/about-organizations),
[teams](https://docs.github.com/en/organizations/organizing-members-into-teams/about-teams),
and [enterprise accounts](https://docs.github.com/en/enterprise-cloud@latest/admin/concepts/enterprise-fundamentals/enterprise-accounts).
Repository roles remain unchanged. The September 23 owner-access decision
explicitly gives every current Organization Owner the owner role on all of that
Organization's existing and future repositories. This is derived authority, not
a migration that copies direct memberships.

## Migration invariants

1. Preserve every content repository ID. Do not hash the new display URL to
   replace a pre-existing repository ID. Snapshot, object, ref, job, and memory
   keys remain untouched.
2. Flatten each existing Workspace into one independent Repository per contained
   code repository. An empty Workspace becomes an unconnected Repository.
3. Copy visibility, policies, archive state, owner, and direct collaborator roles
   to each resulting Repository. Repository assets already stored by content
   repository ID stay in place. Existing invite tokens retain their exact
   original target set; new invitations target one Repository.
4. Keep exact legacy connection paths as aliases to the original immutable ID.
   Ambiguous former container paths must not select a random child repository.
   Authenticate and authorize aliases exactly like canonical addresses.
5. Resolve canonical-name collisions deterministically before writing any data.
   Reserve established two-segment repository paths first. Prefer the former
   child repository name; use the former container name and a stable suffix on
   collision. Emit a migration report showing each mapping.
6. Migrate the old company-space Enterprise to Organization, preserving its
   namespace, administration memberships, policies, logo, and audit history.
   Do not invent an upper Enterprise or duplicate authority during migration.
7. PostgreSQL migration is transactional and serialized across server instances.
   The development filesystem adapter uses a durable migration journal and
   refuses startup if it cannot finish or validate recovery. Preserve the source
   metadata for recovery; never modify transcript objects as part of this move.
8. Historical schema migrations and explicit legacy decoders keep their original
   vocabulary. Active models, ports, adapters, API contracts, UI, and current
   documentation use the new terms.

## Access and concurrency

Repository access combines direct collaborator grants, valid team grants, and
current Organization Owner authority.
Each team grant requires current membership in both the Team and its owning
Organization, and the target repository must belong to that Organization.
An Organization Owner receives the repository owner role even with no direct or
team grant. Use the highest valid role, then apply archive state and
repository/organization/enterprise restrictions. Organization Admin, Member, and
Enterprise Owner roles alone do not grant private context access. Public baseline
access remains explicit. Removing or demoting an Organization Owner immediately
removes inherited authority; independent direct/team grants still apply.
Organization Owners can change the human ownership anchor without changing the
Organization namespace. Personal ownership transfer remains creator-only.

Organization Owner demotion, membership removal, team deletion, and grant revocation must take effect on REST,
MCP, lists, and context writes through the same application policy. Writers lock
the governing records in a consistent order and recheck access inside the write
transaction; a revoked grant cannot authorize a later write. The final owner
cannot be removed, including concurrent demotions. Team roles cannot be used to
modify their own grant or acquire organization administration privileges.

## API and UI contract

- `GET/POST /api/v1/repositories` manages repositories; `repos/{contentID}`
  remains the content-sync API. The two IDs have distinct purposes, not a
  parent/child product hierarchy. Each repository has at most one content ID.
- `GET /api/v1/repository-connections?remote_url=...` resolves a display address
  or a legacy address to the original connection URL and content ID. The CLI
  authenticates and verifies this response before persisting a new connection.
  Existing `.cxt` remotes and offline operations remain usable.
- `/organizations/{id}/teams` and the corresponding members/repositories
  resources manage team membership and repository grants. Team management and
  repository-owner authority are both required to change a team grant.
- `/enterprises/{id}` manages the upper Enterprise. Linking or unlinking an
  Organization requires ownership of both sides, and an Organization can belong
  to only one Enterprise. Enterprise administration does not grant repository
  context access.
- Web addresses are `/{user-or-organization}/{repository}` and
  `/enterprises/{enterprise}`. Exact legacy three-segment repository aliases
  redirect to the canonical page. Retired `/-/` routes remain unavailable.
- Repository lists project `effective_role` from the server. REST, MCP and CLI
  connections apply the same direct/team/Organization Owner access policy. MCP lists the canonical
  address and accepts authorized historical aliases or immutable IDs.

Organization removal requires an explicit `repository_access=revoke|retain`
choice. Both remove all team memberships. `revoke` also removes direct grants;
repositories for which that person remains the ownership anchor must first be
transferred. `retain` keeps direct grants as outside collaboration. Rejoining an
Organization does not silently restore old team memberships. The UI asks for
this choice and the audit records it.

The production identity transaction takes an exclusive PostgreSQL advisory
lock. Context writes take its shared counterpart before repository locks and
revalidate access. This deliberately favors correctness: unrelated identity
mutations serialize, and slow context transactions can delay revocation. Network
calls are outside the identity transaction. Narrower lock domains require a
separate, measured design; the filesystem development adapter does not promise
cross-process transactional rollback.

## Upgrade and recovery

This is a coordinated schema cutover, not a mixed-version rolling deployment.

1. Stop old backend instances and workers. Save a complete database backup and
   the old application image. For the local filesystem adapter, stop the daemon
   and copy the entire data directory, preserving permissions. Protect backups
   as production data; they contain identity and access records.
2. Test the new server against a restored copy first. Inventory content IDs,
   their original remote URLs, boundary memberships/policies, Organization
   records, and content objects. Review the deterministic alias mapping.
3. Start the new backend with migrations enabled. SQL `0052`–`0055` renames the
   active metadata schema and adds Teams/Enterprise. A serialized application
   transaction then splits legacy containers, copies grants/invites, records
   aliases and enforces the one-to-one binding. The marker in
   `ownership_migrations` stores the original-boundary-to-repository mapping.
   Server startup fails if planning or persistence fails; a subsequent startup
   resumes from committed schema versions and retries the data transaction.
4. The filesystem adapter records `ownership-migration-v1.json` before writing.
   On restart it accepts only the recorded before/after contents and refuses
   conflicting edits, path traversal or symlinks. The completed journal is
   retained as `ownership-migration-v1.complete.json`; original legacy directories
   are retained. Objects, snapshots, refs and memories are not rewritten.
5. Verify exact content IDs, original remote URLs, content integrity, member roles,
   both canonical and legacy access, CLI push/pull, and public/private behavior.
   Release the matching frontend and CLI. Existing CLI remotes remain valid;
   older management clients must use the new `/repositories` and
   `/organizations` API contracts.
6. Roll back only while writes remain paused, by restoring the complete pre-cutover
   database/data-directory backup and old binary. Do not run the old binary on
   migrated data or reverse only table names. If new writes have been accepted,
   preserve a second full backup and plan their recovery before restoring.

A malformed source or ambiguous alias is reported as an integrity failure;
never guess ownership or silently select one repository. Historical SQL files,
opaque `ws_`/`ent_` IDs, stored audit descriptions and explicit legacy JSON
readers retain historical vocabulary. New upper Enterprises use `ep_` IDs.
No migration creates billable accounts, charges customers, or enables SSO/SCIM.
Pricing remains free; future pricing is documented separately in
[PRICING.md](PRICING.md).

## Delivery checklist

- [x] Deterministic migration planner and collision/integrity tests
- [x] PostgreSQL migration, restart/idempotency tests, and filesystem recovery
- [x] Active repository and organization terminology across implementation
- [x] Canonical and legacy path resolution with stable CLI repository IDs
- [x] Organization Teams, memberships, repository grants, and revocation
- [x] Enterprise-to-Organizations administration and enforced policy composition
- [x] Repository/organization/team/enterprise UI with Korean and English parity
- [x] REST/OpenAPI and MCP authorization/query consistency
- [x] Real PostgreSQL concurrency and tenant-isolation verification
- [x] Browser E2E and full regression coverage for ownership and compatibility

Release CI, merge and live cutover evidence are recorded in the pull request
linked to issue #251. The upgrade sequence above is required for every existing
deployment; the filesystem development adapter is not a production substitute.
