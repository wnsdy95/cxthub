# Conversation storage

The conversation of a session is stored on the server as one or more
documents, one after another. This page describes how a document is stored and
identified. What a session is,
and who may read which part of it, is described in the
[architecture overview](ARCHITECTURE.md) and the [CLI reference](CLI.md).

## Conversation

- A document is stored as immutable content-addressed chunks and their ordered
  manifest.
- An upload transfers only chunks the destination repository does not own.
- A conversation is cut into documents where the bytes of its records, as the
  tool keeps them, reach 4 MiB. A document that is full never changes, so a
  session that goes on sends only the documents after its last full one.
- Byte-chunk identity is distinct from event identity: one event of a
  conversation can cross a chunk boundary.
- A session's report names its documents in order, with the number of events
  in each, and, for every segment and for the uncommitted part, where the
  range starts in the conversation and how many events it holds. The report
  is stored separately from the documents.
- When a report no longer names a document the report before it named,
  because the session went on or was masked another way, that document is
  deleted when no other report uses it. The pieces it was stored in stay in the server's
  storage until pieces no document uses are removed; with the file store that
  is `cxtd repack`, and the PostgreSQL store has no such step yet.

## Compatibility and implementation boundaries

The default document ID is the SHA-256 of the entire canonical CIR document.
V1 and V2 chunk manifests are physical representations of that same identity.

Verification and publication can work chunk by chunk and avoid cumulative byte
buffers, but they still have to hash the canonical byte stream and check
current chunk integrity. This is **not** a claim of fully incremental
computation or constant-time upload.

Server verification and repository ownership checks are mandatory. Corruption,
cancellation, malformed negotiation, and partial upload failures are errors,
not signals to fall back to another path.

### Explicit conversation roots

The `cxt-manifest-sha256-v1` identity hashes a canonical ordered conversation
manifest instead of the whole document. It is a separate identity scheme,
carried in the `identity` field of a document reference. An absent identity
retains the default meaning. Neither a root hash nor its current conversation
may be relabeled as a default-identity object. The format of the manifest is
specified in [conversation identity](CONVERSATION_IDENTITY.md).

Publication follows this sequence:

1. Confirm the destination repository and its supported identity schemes.
2. Upload missing repository-owned chunks in bounded batches.
3. Submit an identity-bound document finalization job containing the canonical
   manifest. This also applies to a conversation with no events.
4. The worker verifies current owned bytes, ordered occurrences and CIR
   semantics before the document is accepted.
5. Store the session's report, which names the documents, only after every
   one of them is on the server. A report that names a document the repository
   does not hold is refused.

Full current-byte checks remain mandatory. A root makes identity hashing
proportional to the manifest and allows stored chunks to be reused; it does not
make verification or initial indexing constant-time.

Compatible peers declare `X-Cxt-Doc-Identities: cxt-manifest-sha256-v1`. This
declaration does not authenticate the caller, grant access, prove bytes or
enable publication.

A repository takes root documents only when its `required_doc_identity` names
the root identity. That requirement is monotonic: once a repository requires
roots, the requirement cannot be cleared. Read support and permission to
create new roots are separate capabilities. `CXT_CONVERSATION_ROOT_PUBLICATION`
on the server admits new root documents; turning it off stops new root
admission, while compatible binaries keep reading existing roots.

On the CLI, `cxt config capture.identity cxt-manifest-sha256-v1` checks the
configured origin's repository and upload capabilities and then saves the
preference for that destination. It does not enable the server policy. The
default is the whole-document identity (`legacy`). Changing the identity makes
the collector build every conversation again.

Before a repository is opted in, replace all API and MCP instances, workers and
maintenance binaries with compatible versions and drain older instances.
PostgreSQL migrations alone cannot make an older binary safe.

## Orphan chunk cleanup on PostgreSQL

Deleting a document removes its manifest, not its chunks. A chunk nothing names
any more is an orphan: usually the partial tail chunk of an earlier capture of
a conversation that has since grown, or the chunks of an upload that was never
finished. The file store removes them with `cxtd repack`. PostgreSQL removes
them with a background worker; `cxtd repack` does not touch PostgreSQL.

### What is kept

A repository's chunk is live while any of these names it:

- the stored manifest of a document the repository owns,
- the stored manifest of a document one of the repository's snapshots points at,
- a finalization job that is waiting, running or retrying.

An orphan is deleted only after the same ownership row has been seen
unreferenced for a whole grace period of one hour. An upload stores its chunks
before the manifest that names them exists, and the grace period is what keeps
those chunks. It starts when the worker first sees the chunk, which is after
the upload's transaction committed.

The cleanup deletes the repository's ownership row and then the chunk body, and
the body only if no repository owns it any more. Chunk bodies are shared, so a
body another repository still owns stays.

### How it runs

The `chunk-maintenance` worker runs wherever the other maintenance workers run:
in `cxtd` when the API runs its own workers, otherwise in the worker process.
There is nothing to configure, no command to run and no migration.

- Once a minute it visits at most 16 repositories and stops after 45 seconds.
  One repository gets at most 20 seconds.
- A pass over all repositories that own chunks starts at most once an hour. A
  repository with orphans is visited again when their grace period ends. Due
  repositories and the pass take turns.
- One transaction deletes at most 256 chunks. It starts with 16 and is resized
  from the measured cost so that the delete statements take about a second; no
  statement in it may run longer than five seconds.
- The worker tracks at most 16,384 orphans per repository and 65,536 per
  process; a larger backlog is worked off over later visits.

While a deletion transaction runs, pushes to that repository and metered
writes of its namespace wait for it. It waits at most 500 ms for a lock itself
and gives the visit up if the namespace is busy, so keep PostgreSQL's
`deadlock_timeout` above 500 ms (the default is one second).

The first sighting of each orphan is kept in memory. After a restart the grace
period starts again, so a process has to stay up for more than an hour before
it deletes anything. Expect an orphan to be gone about two hours after it
became one. A restart delays the cleanup; it never makes it less safe.

### Why it is safe beside a live server

The deletion transaction takes the repository lock that job enqueue and job
completion take, then the namespace's storage account row, and then the
per-chunk locks that `PutDoc` and the chunk upload take, in the order those
writers take them. A writer that relies on an ownership row without holding
the per-chunk lock share-locks that row instead. The cleanup does not wait for
a chunk lock or a row lock: a chunk another transaction is using is left for a
later visit. With the locks held it reads the manifests and pending jobs
again, so a manifest or job committed in the meantime keeps its chunks. A
writer that names a chunk while it is being deleted waits for the transaction
and then stores the chunk again.

The grace period belongs to the ownership row, not to an upload. An upload
that relies on a row which already existed does not restart it. Two cases
therefore end in a failed upload that the client repeats, never in a damaged
document:

- An upload that takes more than an hour between its first chunk and its
  finalization can lose the early chunks.
- A client that was told the server already has a chunk, or that sent a chunk
  the server already had, can find it gone if that chunk was an orphan near
  the end of its grace period.

In both, the upload is refused, for example with `upload repository-owned
chunks before finalization`, and the next sync uploads the missing chunks.

### Repositories that are skipped

A repository is left untouched, for the whole visit, when one of its document
objects cannot be accounted for completely: an unknown representation, a
corrupt object, a pending job that cannot be read, or a legacy whole-body
document larger than 16 MiB. An object the cleanup does not understand might
name chunks in a way it cannot see. The worker logs the repository and the
reason on each pass, forgets what it was tracking there, and continues with
the next repository.

### Storage accounting

Chunk bytes are counted per owning namespace, not per repository: one
`blob:<hash>` entry for each distinct stored body the namespace's repositories
own. The cleanup deletes ownership rows through ordinary statements, so the
metering trigger adjusts the account in the same transaction and appends a
ledger entry with reason `payload.delete`. A body another repository of the same
namespace still owns stays charged. Reconciliation has nothing to correct
afterwards.

### What an operator sees

A visit that deletes something, or finds orphans in use, logs one line:

```text
chunk cleanup: repository sha256:…: removed 3 orphan chunk(s), freed 412345 stored byte(s) in 3 chunk bodies; 0 left for a later visit, 12 waiting for the grace period
```

A repository that could not be swept logs `chunk cleanup failed: repository
sha256:…: …` with the document or job that stopped it. Visits that change
nothing are silent.

Reclaimed bytes are also in the storage ledger, which survives restarts:

```sql
SELECT namespace_id, count(*) AS bodies, -sum(delta_bytes) AS freed_bytes
FROM storage_usage_ledger
WHERE reason = 'payload.delete' AND object_key LIKE 'blob:%'
  AND occurred_at > now() - interval '1 day'
GROUP BY namespace_id ORDER BY freed_bytes DESC;
```

The ledger also records deleted documents under the same reason, and it has no
entry for a repository outside any namespace. Storage usage in the product
shows the lower total.

### Not covered

- Memory chunks (`memory_chunk`) are a separate kind and are not swept.
- A body that no ownership row references at the time the cleanup is deployed
  is never visited, because the cleanup starts from ownership rows.

## Required validation

1. Appending to a conversation changes only the new and tail chunks; chunks
   already stored keep their identities.
2. Changed, missing, unowned, reordered or malformed chunks cannot produce a
   valid publication. Cancellation and expired job leases publish nothing.
3. Measure newly transferred bytes, bytes read and hashed, parsing,
   allocations, indexing and completion wait separately. Reduced allocation is
   not proof of reduced cold indexing time or end-to-end transfer latency.
