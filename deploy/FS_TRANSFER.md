# Rehearse an existing FS dataset on PostgreSQL

The Render API and MCP services share PostgreSQL. This procedure prepares and
checks an existing local FS dataset **without provisioning or deploying** either
service. It does not switch the running development server's storage.

## Supported source

`cxt-fs-rehearsal` is an offline operator command, built with the `postgres` tag.
It supports the current personal-repository FS layout: users, repositories,
memberships, invitations, sessions, aliases, context objects, refs, history,
pending captures, reflogs, document/PR jobs and Git evidence. It preserves IDs,
natural and overlay parents, memory links, job states and lease versions. It does
not replay branch or PR commands during import.

This is not a universal migrator. Organization, team, Enterprise, OAuth,
GitHub-installation, secrets and notification datasets need explicit additional
mappings. If such records or another unknown file category are present, the
command stops before importing runtime data. Do not delete unsupported files to
bypass that check. The split-service release alone does not migrate FS data.

Derived document indexes and Git comparison lookup files are rebuilt on demand.
Old Workspace records and the ownership-migration completion marker remain in
the backup. Membership files left behind by that completed migration are not
reactivated: only the memberships used by the current Repository runtime are
imported. Legacy membership records without the completion marker are rejected.
PostgreSQL timestamps retain microsecond precision. Git observation records
without a stored receipt time use their frozen source file's modification time.

## Freeze and protect the source

1. Stop the FS server and every writer sharing its data directory. Do not merely
   pause HTTP traffic while background workers continue writing.
2. Copy the entire data directory into a private backup, retaining permissions
   and modification times. APFS clones are suitable on macOS when the copy is
   made while writers are stopped. Restrict the backup directory to its owner.
3. Restart the original server unchanged. The backup stays frozen; run no FS
   server against it, since startup recovery may modify files.
4. Keep an independent file-size/SHA-256 inventory and backup. The command also
   hashes every source file before and after import and rejects changes,
   symlinks, missing repository identities and corrupt content.

The source contains private conversations, member data and authentication
records. Never commit it, attach it to an issue, or put it in CI fixtures.

## Run locally

Start PostgreSQL 16 on loopback. Create a **new empty database**, with no API,
MCP server or worker connected to it. Run from the repository root:

```bash
go build -C backend -tags postgres -o ../bin/cxt-fs-rehearsal ./cmd/cxt-fs-rehearsal
export CXT_REHEARSAL_DSN='postgres://<local-user>@127.0.0.1:<port>/<empty-rehearsal-db>?sslmode=disable'
bin/cxt-fs-rehearsal --source /absolute/private/frozen-backup > /absolute/private/dry-run.json
```

The command accepts only loopback connection hosts (including fallback hosts),
rejects the default `postgres` database and checks that all runtime tables are
empty before applying schema migrations. Schema tables remain after a dry run;
all imported runtime data rolls back. No cloud credentials or `.env` files are
loaded, and no application workers run.

After a successful dry run, retain the verified copy in that same empty local
database:

```bash
bin/cxt-fs-rehearsal --source /absolute/private/frozen-backup --apply > /absolute/private/applied.json
```

Import uses one serializable transaction and locks runtime tables before
rechecking emptiness. Failures and cancellation roll back data writes. A second
apply to a populated target is rejected; the command never truncates an existing
database. Reports contain counts, total bytes and an aggregate source hash, not
conversation text or credentials.

## Verify before accepting the rehearsal

- Compare report source hashes and category counts with the frozen inventory.
- The importer verifies document/component/memory hashes, all snapshot metadata,
  missing parents, graph cycles and the source/target sync manifest.
- Compare effective membership roles and repository ownership; obsolete roles
  must not return. Verify repository path aliases and the retained job states.
- Exercise repository listing, graph/history, paged conversation and memory reads
  through separate API and MCP pools. Use a worker-free HTTP harness for a copied
  production-like dataset; ordinary API startup may dispatch copied jobs.
- Check cold and warm reads. Derived caches were intentionally not trusted or
  copied, so an initial read can rebuild an index.
- Back up the accepted local PostgreSQL copy with `pg_dump --format=custom` to a
  private file and rehearse restore into another empty local database.

Synthetic regression tests run against a disposable PostgreSQL instance:

```bash
CXT_TEST_DSN='<local test database DSN>' \
  go test -C backend -tags postgres ./internal/adapters/store -run 'TestPGFrozen|TestFrozen'
```

The test role needs `CREATEDB`; each import test creates and removes its own
database. Never point these tests at production.

## Actual cutover is a separate operation

The development server may have accepted more records since the backup. Do not
deploy the old rehearsal dump as if it were current. At the approved cutover,
freeze writers again, take a fresh backup, repeat import/verification and transfer
the accepted PostgreSQL dump through the chosen provider's private access path.
Keep the original FS backup and rollback instructions until acceptance completes.

Start MCP and API against the **same** verified PostgreSQL database, Firebase
project and public origin. Follow [Render deployment](RENDER.md) for route order,
readiness and rollback. Provisioning, production transfer, DNS, GitHub/Firebase
registration and external email/webhook smoke tests are outside this rehearsal.

For the local development cutover and private macOS service configuration, use
[Independent local PostgreSQL services](LOCAL_POSTGRES.md).
