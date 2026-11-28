# Repository catalog state and restore

The catalog is a per-repository epoch in the database. The server no longer
serves a catalog to clients: the routes `POST /repos/{repoID}/pull/catalog` and
`/pull/catalog/merkle` are not registered, and the CLI neither reads nor writes
`.cxt/catalog-cache`. What matters to an operator is the epoch.

## What still exists

- Migration 0171 drops the change journal (`repository_catalog_changes`), the
  capture triggers that wrote it, and `cxt_prune_catalog`.
  `repository_catalog_state` stays: a trigger on `repos` creates its row for
  each new repository, and `cxt_reset_catalog` rotates its epoch.
- The server reads `repository_catalog_state.epoch` together with a document
  finalization job and carries it as the repository's epoch for that job.

A catalog row is metadata, **not an availability proof**, a retention lease or
a verified document receipt.

## Maintenance and restore

This SQL maintenance function is operator-controlled; nothing schedules it:

- `SELECT cxt_reset_catalog('<repo-id>');` rotates the repository's epoch and
  resets its head and floor to zero, under a lock on `repos`. It is a
  maintenance operation, not an online recovery shortcut.

For database restore or out-of-band import recovery, stop source writers
**before** recovery, keep them stopped after restore, run `cxt_reset_catalog`
for every restored repository, and commit the maintenance transaction before
resuming service. A restored database cannot detect its own rewind; restoring
an old epoch without rotating it can make work that was accepted before the
recovery appear current. The reset locks `repos` for every repository, so
schedule downtime accordingly. If recovery fails, keep service stopped until
the data is consistent.

`scripts/verify-postgres-recovery.sh` rehearses exactly this on two dedicated
validation databases: it dumps the source, restores into an explicitly empty
target, compares every public table's row count and content digest, then
calls `cxt_reset_catalog` for every restored repository in one transaction and
checks that every epoch differs from the source. See the
[collaboration transaction contract](COLLABORATION_TRANSACTIONS.md#reproducible-validation)
for how to run it.

Never `TRUNCATE` `repos`, `repo_blobs` or `repository_revisions` (they refuse
it), or edit `repository_catalog_state` by hand while serving clients.
