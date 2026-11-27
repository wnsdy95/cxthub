# Repository catalog state and restore

The catalog is a per-repository journal in the database, with an epoch and a
sequence. The server no longer serves it to clients: the routes
`POST /repos/{repoID}/pull/catalog` and `/pull/catalog/merkle` are not
registered, and the CLI neither reads nor writes `.cxt/catalog-cache`. One part
of the catalog still matters to an operator: the per-repository epoch.

## What still exists

- The migrations still create the catalog tables (`repository_catalog_state`,
  `repository_catalog_changes`), their capture triggers on the tables of the
  earlier approach, and the SQL functions `cxt_reset_catalog` and
  `cxt_prune_catalog`. No migration in the current catalog drops them.
- The server still reads `repository_catalog_state.epoch`. It reads it
  together with a document finalization job and carries it as the
  repository's epoch for that job.
- No part of the current server serves journal entries to a client.

A catalog row is metadata, **not an availability proof**, a retention lease or
a verified document receipt.

## Maintenance, retention and restore

These SQL maintenance functions are operator-controlled; nothing schedules
them:

- `SELECT cxt_reset_catalog('<repo-id>');` rotates the repository's epoch,
  clears the old journal, resets its head and floor to zero, and reseeds
  current images under table locks. It is a maintenance operation, not an
  online recovery shortcut.
- `SELECT cxt_prune_catalog('<repo-id>', <floor>);` advances a repository's
  retention floor atomically with pruning.

For database restore or out-of-band import recovery, stop source writers
**before** recovery, keep them stopped after restore, run `cxt_reset_catalog`
for every restored repository, and commit the maintenance transaction before
resuming service. A restored database cannot detect its own rewind; restoring
an old epoch without rotating it can make work that was accepted before the
recovery appear current. The reset's table locks cover all repositories, so
schedule downtime accordingly. If recovery fails, keep service stopped until
the data is consistent.

`scripts/verify-postgres-recovery.sh` rehearses exactly this on two dedicated
validation databases: it dumps the source, restores into an explicitly empty
target, compares every public table's row count and content digest, then
calls `cxt_reset_catalog` for every restored repository in one transaction and
checks that every epoch differs from the source. See the
[collaboration transaction contract](COLLABORATION_TRANSACTIONS.md#reproducible-validation)
for how to run it.

Never `TRUNCATE` the source tables, disable or bypass the capture triggers, or
manually edit the journal or state tables while serving clients. Pruning bounds
retained journal history only when an operator invokes it; it is not object or
blob garbage collection and gives no object retention guarantee.
