# Catalog synchronization

`POST /api/v1/repos/{repoID}/pull/catalog` exposes version **1**, scope
`sync-metadata-v1`, as an optional PostgreSQL metadata query. Pull permission is
checked on every request, including continuation pages and empty deltas.
Filesystem storage explicitly returns HTTP 501 `catalog_unsupported`. Existing
manifest and object routes remain available. The CLI uses catalog acquisition
for snapshot discovery and complete pull metadata when the freshly authorized
repository view advertises `catalog_version: 1`. Absent/zero means the existing
full-manifest protocol; unknown versions and errors after advertising version 1
fail without fallback. Selected-branch dependency plans and viewer-readable
manifest calls keep their existing contracts. Merkle reconciliation remains a
subsequent stage. Reduced metadata transfer does not by itself establish faster
end-to-end document fetches.

## Scope and images

The journal records original stored metadata images in the same transaction as
the source mutation. It covers four entity kinds:

| Kind | Key | Value |
| --- | --- | --- |
| `snapshot` | Snapshot ID | Stored snapshot metadata, including mutable attachment and graft fields, without derived `branches` |
| `ref` | JSON array `[kind,name]` encoded as a string | Raw ref, including lifecycle tags and branch identity |
| `history` | History event ID | Full stored `history.event` |
| `protocol` | Repository ID | `{"context_protocol": integer}` |

Ref keys are JSON arrays, not a delimiter convention; parse them as JSON rather
than depending on whitespace. Raw lifecycle refs must be projected under the
recorded context protocol before they can be used as visible refs. Snapshot
parent order and full history event fields are preserved.

Memory bodies, legacy memory metadata, document/attachment bytes, blob grants,
pending state, authorization data and derived graph data are outside this scope.
A metadata checkpoint is **not an availability proof**, a retention lease, a
verified document receipt, or applied client state. Consumers must still fetch
and validate objects and dependencies through the existing protocols. Permission
may change between any two requests; a cursor grants no access.

## Baselines, deltas and checkpoints

Start a baseline with `{"version":1}`. `limit` defaults to 256 when omitted or
zero; the maximum is 1000. Negative limits and larger limits are invalid. Each
request must be a single JSON object, at most 64 KiB, without unknown fields.
Field names are case-sensitive; duplicate keys and null field values are invalid.
All four checkpoint fields are required, including a possibly zero sequence.

A baseline fixes `through` at the committed catalog head on its first page and
returns the latest **nondeleted** journal image for each `(kind,key)` at or below
that sequence. It reads immutable journal images rather than current mutable
source rows. Baseline order is `(kind,key)` using PostgreSQL C collation, not
mutation order. Sequence zero is valid for seeded baselines. Stage the baseline
as a replacement metadata image so entities absent from it are removed.

A partial page has `next_cursor` and no `checkpoint`. Resume with
`{"version":1,"cursor":"<next_cursor>"}`; retain the opaque base64url string
exactly. The adapter binds it to repository, scope, version, epoch, retention
floor, mode, fixed upper sequence and pagination position. Clients must not
construct or interpret it. Newer writes do not change the selected upper bound.

Only the final page returns a checkpoint:

```json
{
  "version": 1,
  "repo_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "epoch": "e66213bd-003e-47d1-b965-e690036bfb08",
  "sequence": 42
}
```

Request the next delta by sending that object as `after` along with
`"version":1`. `after` and a nonempty `cursor` are mutually exclusive. A delta
returns entries strictly after the checkpoint through a newly fixed committed
head, ordered by `(sequence,kind,key)`. Each nondeleted value replaces that
entity's metadata image; `deleted:true` removes it and has no value. An empty
delta still returns a final checkpoint. All page entry arrays are non-null.

The catalog sequence is allocated under a repository state-row lock and shares
the source transaction. Multiple changes in the same repository transaction
share a sequence; repeated changes to one key coalesce to its last image. A
second writer cannot publish a higher sequence ahead of an uncommitted earlier
writer. Rollback also rolls back the journal and counter.

Page limits may split one transaction across multiple pages. Never adopt a page
boundary or an entry's sequence as a complete checkpoint. Stage all pages, then
atomically apply the completed metadata image/delta and the **final** checkpoint
whose repository, epoch and sequence match the page's `repo_id`, `epoch` and
`through`. Partial pages never carry a checkpoint, and final pages have no next
cursor. Catalog metadata alone does not justify updating verified/applied state.

## Errors and restart behavior

| HTTP | Code | Meaning |
| --- | --- | --- |
| 400 | `bad_request` | Invalid request version/shape, limit, checkpoint fields or malformed cursor |
| 401 / 403 | Existing authorization codes | Pull permission is absent or revoked; do not bypass the gate |
| 404 | `not_found` | Repository/catalog scope does not exist |
| 409 | `reset_required` | Wrong repository/epoch/scope, unsupported checkpoint/cursor version, future or pruned progress, or continuation retention floor changed |
| 413 | `payload_too_large` | Request exceeds 64 KiB |
| 415 | `bad_request` | JSON content type required |
| 501 | `catalog_unsupported` | Store lacks the optional catalog capability |

On `reset_required`, discard the incomplete staged run and request a fresh
baseline. Do not turn a failed/partial page into successful progress. A new
baseline replaces the metadata cache only when its final checkpoint is reached.
Unsupported storage can use the established full-manifest protocol; it does not
claim catalog consistency by scanning mutable rows. Errors other than explicit
unsupported/reset responses retain their existing semantics.

## Maintenance, retention and restore

Migration `0074_catalog_changes.sql` seeds existing rows at sequence zero and
installs capture triggers while holding locks on all four source tables. The
baseline and schema change commit together. These SQL maintenance functions are
operator-controlled; this change adds **no automatic GC scheduler**:

- `SELECT cxt_prune_catalog('<repo-id>', <floor>);` advances a repository's
  retention floor atomically with pruning. The floor must be between the current
  floor and head. It retains the latest anchor for every key at or before the
  floor, including deletion anchors, plus newer changes. Therefore a fresh
  baseline remains reconstructible. Deltas with `after.sequence < floor` require
  reset. Every baseline or delta continuation cursor resets if its recorded
  floor differs from the current floor, even if its upper sequence is newer.
- `SELECT cxt_reset_catalog('<repo-id>');` rotates the epoch, clears the old
  journal, resets the head and floor to zero, and reseeds current source images
  under locks on `repos`, `snapshots`, `refs` and `context_history`. It is a
  maintenance operation, not an online recovery shortcut. Old checkpoints and
  continuation cursors then require a fresh baseline.

For database restore or out-of-band import recovery, stop serving catalog clients
and source writers **before** recovery, keep them stopped after restore, run
`cxt_reset_catalog` for every restored repository, and commit the maintenance
transaction before resuming service. A restored database cannot detect its own
rewind; restoring an old epoch and sequence without rotating them can make stale
checkpoints appear valid. The reset's table locks cover all repositories, so
schedule downtime accordingly. If recovery fails, keep service stopped until
source data and reseeded catalogs are consistent.

Never `TRUNCATE` source tables, disable/bypass capture triggers, or manually edit
journal/state tables while serving clients. Use ordinary transactional source
mutations with capture enabled, or an offline recovery followed by epoch reset.
Pruning bounds retained history only when an operator invokes it; it is not
object/blob GC and gives no object retention guarantee.

## CLI acquisition cache

The CLI keeps an endpoint/repository-scoped catalog cache in `.cxt/catalog-cache`,
separate from snapshot storage, verified remote observations, refs, history
imports and worktree selection. Tokens and URL credentials are not persisted.
Each received page is immutable and checksummed. Unfinished pages form an
immutable linked chain; completed indexes live in a separate immutable descriptor.
A bounded head with a monotonic generation points to their roots and records the
last complete checkpoint separately. Partial pages do not rewrite the growing
list of all previously received page hashes.
Publication is atomic and compare-and-swap guarded across CLI processes. A losing
writer returns a synchronization conflict rather than overwriting the winner.

The CLI starts with 256 entries per page and a 32 MiB response bound. If a
response exceeds that bound, it retries the same checkpoint/cursor with half the
entry limit, down to one. Oversized responses never advance progress; a single
entry that still cannot fit reports an explicit error. Other errors do not
trigger this retry. Empty legacy default branch names remain valid for capability
negotiation.

Every invocation checks current capability and pull authorization. A warm empty
delta still contacts the server. An interrupted run resumes its opaque cursor,
then requests a fresh delta once to catch up beyond the old fixed bound. Only a
validated final page can publish the replacement baseline or complete delta.
A reset abandons unfinished progress but retains the last complete image until
the new baseline finishes. Repeated resets fail instead of retrying forever.

Reads validate current cache bytes and metadata identities; malformed or corrupt
records are errors, not permission to reconstruct or overwrite silently. Page
logs compact at an adaptive threshold so a small update does not rewrite the
entire image each time. Unreferenced pages are retained. Current full-image local
reads and validation remain linear in catalog size; this is not Merkle lookup.
Document, chunk, memory, settings and history dependency validation still runs
through the existing synchronization paths. No metadata checkpoint can prove
that an object is available or has been applied locally.
