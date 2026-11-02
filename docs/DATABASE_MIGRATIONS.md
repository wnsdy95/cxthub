# Database migrations: operator ownership and verify-only startup

This is the MIG-1 release contract. The migration image, operator command, store
verification, and production startup changes must be integrated and validated
together before use. Adding this packaging does not establish that those changes
are already implemented or that a deployment has been performed.

## Runtime contract

All production PostgreSQL roots verify the schema before serving traffic or
starting background work: API, MCP, and every worker, not only bounded execution
profiles. They call `Store.VerifyMigrations(ctx context.Context, directory string)
error`, never runtime `ApplyMigrations`. There is no runtime environment-variable
fallback that enables migration application or automatic DDL. A fresh database
must be migrated explicitly before any production root starts.

Verify-only startup is not a read-only application: authorized business DML
continues under the existing rules. Local filesystem development remains
unchanged; filesystem migration verification is a no-op, not a substitute for
production PostgreSQL verification.

`CXT_MIGRATIONS_DIR` identifies the release's migration bundle. Verification must:

- Match the compiled release catalog before opening a database transaction.
  Matching an old database to an old directory is not enough for a newer binary.
  Both verification and public operator apply enforce this check; changing an
  environment variable cannot disable it.
- Read the exact, nonempty, complete bundle and compare every SQL filename and
  SHA-256 checksum with `public.schema_migrations`, not merely the highest version.
- Reject absent or unreadable files, empty or incomplete bundles, missing history
  or migrations, unknown history entries including newer versions, blank stored
  checksums, and checksum mismatches. A matching version count is insufficient.
- Require the supported `public` namespace. A custom schema or shadow relations
  selected through `search_path` cannot stand in for the public relations.
- Verify the original `repository-ownership-v1` receipt in
  `public.ownership_migrations` and the existing accounting security boundary;
  SQL history alone is not proof that these obligations were completed.
- Use the existing `cxt-schema-migrations` advisory lock in a read-only transaction,
  sharing the migration serialization boundary with operator application. Do not
  introduce a second lock or repair anything during verification.

Schema incompatibility is reported through `store.ErrSchemaMismatch` and prevents
startup. Connection, file-access, lock, or cancellation failures also prevent
startup; none authorizes continuing or applying migrations. Verification does not
create history, backfill checksums, manufacture an ownership receipt, or repair
accounting objects.

The catalog digest hashes the sorted sequence of each SQL filename, a NUL byte,
its lowercase hexadecimal SHA-256 content checksum, and a newline. When adding a
reviewed migration, update the compiled catalog digest and its release test in
the same change, and ship the identical bundle in every release image. Do not
update the digest merely to accept an unexplained bundle or history mismatch.
The private historical migration helper is only for isolated migration fixtures
and offline tooling; production roots do not receive its apply capability.

## Settings schema release 152

Migration `0152_settings_codex.sql` expands the existing validated
`repo_settings_kind_check` from `claude/agents` to `claude/agents/codex`, matching
the existing domain contract. Unknown kinds remain rejected. The historical
151 SQL files, checksums, applied timestamps, settings objects and repository
ownership receipts must remain unchanged. The migration's constraint DDL and
its own history receipt commit together; cancellation or receipt failure must
leave the previous constraint intact.

The complete 152-file catalog digest is
`42c721b3d691c2ff3f2b6d717da9a879246fe031ae464885a317c3ec14937f5b`.
Ship that exact bundle with the corresponding API, MCP, worker and operator
artifacts. Current verify-only startup also checks the public table and its
validated, local, exact kind constraint. A receipt without the expected DDL,
a missing or unvalidated constraint, or an expanded unknown kind is not a
compatible schema. Verification does not repair these conditions.

This upgrade requires the full offline sequence below. A current 152 binary
must not serve a 151 database, and a historical 151 binary must not serve a 152
database. This is a schema transition, not the same-catalog protocol handoff
qualified earlier. Reusing protocol 3 does not make the catalogs compatible:
the existing control identity already binds the compiled catalog.

There is no automatic destructive schema downgrade. Do not remove Codex
settings, edit migration receipts, rewrite historical SQL, or point an old
binary at a substitute bundle to force startup. Recovery requires an artifact
compatible with schema 152, or a separately verified pre-upgrade backup recovery
that explicitly accounts for writes after the backup. Local fixture tests are
not evidence that a production backup or recovery has been tested.

## Bounded process control compatibility

Canonical budget equality alone is not release compatibility.
`Budget.Fingerprint()` and `ProcessAdmissionConfig.BudgetFingerprint` remain the
unchanged raw budget fingerprint. `store.ProcessAdmissionFingerprint` derives the
runtime control identity as the 43-character, unpadded base64url SHA-256 of
`fmt.Sprintf("cxthub/process-admission/v%d\x00%s\x00%s", 3, releasedMigrationCatalogSHA256, budgetFingerprint)`.
The separators are actual NUL bytes; the budget input is its canonical,
unpadded base64url string encoding of a 32-byte hash, not the decoded bytes.
Invalid or noncanonical budget hashes are rejected. The catalog input is the
existing compiled released SQL catalog digest described above, not an operator
override. `OpenProcessAdmission` uses this bound identity for its control session
and controller fingerprint; it does not replace the raw budget fingerprint.

Every bounded API, MCP, and worker must match that protocol-v3/catalog/budget
identity for the cohort to become ready. No environment override, new ledger,
or additional admission gate is introduced. Existing role singleton locks,
namespaces, prefixes, pools, and gates remain unchanged: one process per role,
four active operations (API two, MCP one, worker one), two shared bulk slots with
the existing worker reserve, and the original connection/reserve and spool caps.
Budgets, leases, and safety checks are not relaxed.

Protocol3 includes current repository-policy pinning for context writes and
first registration while direct repository metadata commands use target-local
authority locks. Protocol2 writers do not provide all these policy pins. They
must not be mixed with protocol3 writers even when the SQL catalog and budget
are identical. Exact local v2/v3 executable handoff qualification is recorded
in the incremental publication plan's R4-A-1 evidence; it is not authorization
to roll out an arbitrary old image or to skip the full drain below.

Before switching protocols or releases, even without SQL changes, explicitly
drain and stop **all** API, MCP, and worker roles, including old replicas and
unmanaged binaries; prevent automatic restarts. Follow the offline sequence
below: require the exact release's operator schema check and runtime-principal
verification, then start only a matching cohort. Observe readiness and useful
API, MCP, and worker operations before resuming traffic. Mixed old/new
same-budget cooperating peers are intentionally unready; there is no promise of
zero-downtime rolling upgrades, automatic downgrade, or history rewriting.

Any local compatibility result covers only the exact old/new binary pair
actually tested, not arbitrary old binaries. Image, container, and deployment
qualification, detection of noncooperating processes, and the embedded/default
transition remain open. Source-specific local execution evidence is recorded in
the incremental publication plan's SET-1 section. This operator contract does
not qualify a production image, deployment, or backup recovery.

## Separate database principals

The operator uses a privileged migration principal through `CXT_POSTGRES_DSN`.
It must have the reviewed ownership and DDL authority required by the existing
migrations and repository-ownership conversion. Keep this credential out of API,
MCP, and worker environments. Operator-only describes the credential and release
procedure; the image is not a replacement for database access control.

Runtime principals use separate, least-privileged DML credentials. Allow the
reads needed to verify public migration history, ownership receipts, and security
metadata, alongside the existing business permissions. Do not grant runtime
principals migration-history writes, schema/object ownership, DDL or `CREATE` in
`public`, or membership that permits assuming the migration/accounting owner.

Preserve the original repository-ownership semantics and receipt. Keep the
accounting tables, triggers, trusted owners, `SECURITY DEFINER`/`SECURITY INVOKER`
modes, fixed function `search_path`, and existing revocations intact. In
particular, do not grant runtime code direct mutation authority over protected
legacy-byte usage accounting or bypass its trusted trigger path. This separation
does not change limits, authentication/authorization, hashes, leases, current
authority, accounting semantics, or other existing security rules.

## Operator image and invocation

`deploy/Dockerfile.migrate` builds only the PostgreSQL-tagged `cxt-admin` executable
using the same pinned Go builder and distroless runtime as `deploy/Dockerfile`.
It bundles the repository's exact `schemas/db/migrations/` directory at
`/app/migrations` and runs as `nonroot:nonroot`. It contains no server or worker
executable, exposes no port, and starts no listener or background worker. There
are no credential build arguments or embedded database credentials.

Build context must be the source repository root, not `deploy/` or `backend/`:

```sh
docker build -f deploy/Dockerfile.migrate -t cxt-admin:reviewed-release .
```

This is an operator instruction, not evidence of a successful build. Prepare and
review artifacts separately from the offline database window. Use an immutable
image reference for the approved release, supplied as `CXT_MIGRATE_IMAGE`, and
inject `CXT_POSTGRES_DSN` privately at execution time. Do not put credentials in
image layers, build arguments, command-line DSN literals, or source control. Do
not replace the bundled migrations with a host directory or another release's
bundle.

The entrypoint is `[/app/cxt-admin, migrate]`. With no arguments it is check-only:

```sh
docker run --rm --read-only --env CXT_POSTGRES_DSN "$CXT_MIGRATE_IMAGE"
```

Application requires the explicit operator flag:

```sh
docker run --rm --read-only --env CXT_POSTGRES_DSN "$CXT_MIGRATE_IMAGE" --apply
```

Do not append another `migrate` subcommand: it is already in the entrypoint. The
image sets `CXT_MIGRATIONS_DIR=/app/migrations`; changing that directory does not
authorize runtime application. Check-only on a fresh or not-yet-upgraded database
must fail rather than initialize it. Reviewed apply may advance valid history
with missing migrations through the existing guarded owner path; it is not a way
to accept unknown versions, rewritten files, or blank/mismatched checksums.

## Offline deployment sequence

Offline means application traffic and background processing are stopped; the
authorized operator still needs controlled database connectivity. This procedure
does not provision a database, database roles, a cloud service, or a deployment.

1. Review the exact SQL bundle, ownership/accounting changes, compatible API/MCP/
   worker artifacts, credentials, and recovery plan. Keep their migration bundles
   identical. Arrange explicit operator approval; do not attach apply to service
   startup, a Render `preDeploy` command, or an automatically activated job.
2. Drain and stop **all** API, MCP, and worker processes that can reach the
   database, including old replicas and unmanaged binaries. Let in-flight work
   finish under the existing lease rules; do not bypass leases or authority
   checks. Prevent supervisors from restarting old processes during the window.
3. Take a recoverable backup of the quiesced database and preserve the required
   role/ownership configuration and matching release artifacts. Confirm the
   restore plan before applying. The advisory lock coordinates migrations, not
   arbitrary application traffic, so it does not replace draining.
4. With the reviewed privileged migration principal injected as
   `CXT_POSTGRES_DSN`, run the approved image with `--apply`. Inspect completion
   and retain its outcome without credentials. Stop on any error or uncertain
   outcome; do not start services or rewrite history to force progress. Existing
   per-migration transactions do not make the entire bundle one atomic operation.
5. Run the same image without arguments to check the complete result, including
   the ownership receipt and accounting boundary. Then check with the intended
   runtime DML principal(s) to confirm they can verify without owner privileges.
   Require success before starting production roots. Do not cure a permission
   failure by giving runtime processes the operator credential.
6. Start only the reviewed compatible API, MCP, and worker artifacts with runtime
   credentials, as a matching control cohort for bounded profiles. Every
   PostgreSQL root must pass its own verify-only startup before resuming its
   normal work. Observe readiness and useful operations across all roles before
   resuming traffic; keep migration credentials out of these environments.

Uncontrolled old binaries continuing to run against the migrated database remain
unproven. A successful new startup check does not validate old-process behavior,
rolling upgrades, or arbitrary mixed-version operation.

## Failure and rollback

The exact file/checksum/unknown-newer guard intentionally fails closed when an
older image sees a newer schema. Reverting an image is therefore not an arbitrary
schema downgrade and may prevent startup. Check-only never reverses migrations;
`--apply` is not a downgrade command either.

On failure, keep roots stopped and review the actual committed history with the
operator. Recover only through an approved compatible release/forward repair or
a coordinated restore of database, roles, and matching images from the reviewed
backup. Do not delete history rows, fill blank checksums by hand, edit released
SQL, forge ownership receipts, relax accounting security, or disable verification
to make an older image start. A forward repair still requires an approved bundle
and a compatible image/schema pair.

## Validation and release boundary

The backend distribution exports `Dockerfile.migrate` and this runbook alongside
the unchanged backend source and complete SQL bundle. The API and MCP images
remain service-only; only the separate operator image provides the migration
entrypoint. Export wiring or a successful build is not deployment approval.

Before release, validate the actual operator command, restricted runtime startup,
missing/mismatched schema refusal, current schema checksums and ownership,
original accounting guards, cancellation, and the existing service lifecycle
and historical migration tests. Container readiness must be tested only after
an explicit operator application; a fresh database must not be initialized by
starting API or MCP.

No Blueprint, cloud resource, credential grant or pre-deploy activation is
performed by these source changes. Image/schema compatibility, production
credential separation, backups, coordinated drain and rollback require a
reviewed release procedure. Do not infer arbitrary old-binary compatibility,
cloud readiness or full physical-resource capacity from local tests.
