# Live capture and repository updates

Live capture preserves the native provider transcript and publishes a per-session
pending pointer. It does not commit context, advance a branch or inject another
session's conversation into an active provider window.

## Session archives

The **Archive** action immediately after **raw** archives the entire selected
session for the repository, whether its captures are committed, unpushed or
uncommitted. Only maintainers and owners can archive or restore. All readers can
open the original conversation from **Archived sessions**, below **Archived
branches** in the graph sidebar, without restoring it first.

Archiving is a server-persisted visibility overlay, not a deletion, commit,
branch archive or memory invalidation. The ordinary lists and graph hide the
session, but original content, memory attachments, refs and verified PR joins
remain unchanged. CLI and MCP reads and AI context assembly still use the
original history. A future capture of the same provider/session stays archived.
Different providers with the same native session ID remain distinct. Legacy
records with no session ID can only be archived one snapshot at a time.

The archive list exposes the recorded parent session and historical main code
position where evidence is available. Missing or ambiguous provenance is shown
as unknown; the current main tip is never substituted for a historical baseline.
An older grafted record without immutable origin evidence is also unknown;
publishing its parent later does not establish where the session started.
Opening an archive prefers a terminal capture in that session's natural lineage,
then the latest recorded time, so equal-time batch uploads do not open an ancestor.
Displaying an archived session does not restore it; **Restore** explicitly
returns it to the normal view. Existing pending-list dismissals are a separate
legacy setting, not automatically converted or deleted.

`POST /repos/{repoID}/snapshots/{id}/archive` accepts `{"archived":true}` or
`{"archived":false}`. PostgreSQL serializes the command with repository writes,
rechecks management permission and commits the visibility change, graph revision
and applicable organization audit event atomically. Both full and live views
return the same `archived_sessions` projection. The graph's full evidence remains
available before the browser applies visibility filtering. Archived snapshots
are retention roots and are protected from sliding-capture garbage collection.

## Incremental native projection

The observer checks source size, modification time and masking-policy fingerprint.
It batches changes, completes a durable pending upload before another capture,
and retains the existing independently bounded capture and upload commands.

Claude and Codex codecs carry envelope state and event sequence across complete
JSONL records. The checkpoint stores the processed byte offset, prefix hash,
masked envelope, event count, document hash and policy fingerprint. It is a
disposable, checksummed hint under `.cxt/capture/projections`, not a commit receipt.
Only a durable content-addressed document can become its next checkpoint.

Closed canonical event chunks are reused. Only new events are decoded, masked
and canonicalized. A bounded-memory native prefix hash detects earlier rewrites;
rotation, truncation or a changed masking policy rebuilds the projection. A partial
last JSONL record stays unconsumed for a later pending capture. An explicit
non-pending save rejects an incomplete record. Missing cached documents invalidate
the hint and retry; corrupt content-addressed chunks fail integrity verification.

Snapshot identity remains the hash of the **entire canonical document**, including
its changing envelope. Prefix integrity reads and whole-document hashing therefore
remain proportional to transcript size. This optimization removes repeated JSON
interpretation/masking/canonicalization; it does not promise constant end-to-end
cost or change the identity protocol. Initial captures still process the full source.

## Durable repository revision

`repository_revisions` stores independent graph and pending counters in PostgreSQL.
The published pointer change and its revision commit in the same transaction.
A rollback publishes neither. Object upload alone does not publish a graph revision;
ref/history publication or changes to existing metadata do. Immutable memory blobs
remain outside a losing attachment CAS, preserving the existing recovery contract.
Objects-only CLI uploads skip ref reconciliation. Explicit ref synchronization
retains empty-batch pending reconciliation; legacy empty batches advance only the
pending revision, since they cannot move a branch.

- `GET /repos/{repoID}/view`: initial complete graph generation plus revision.
- `GET /repos/{repoID}/pending-view`: pending pointers and their unique target
  snapshots, from one committed generation. It never traverses older history.
- `GET /repos/{repoID}/revision`: cheap current counters, for fallback recovery.
- `GET /repos/{repoID}/changes`: authenticated SSE notifications of current counters.

Each server shares one small cursor query per observed repository every two seconds
across its subscribers. The database query count does not multiply with browser
count on that server. Unobserved repositories have no watcher. Every connection
receives current durable counters, so missed events, another server and process
restart converge without relying on an in-memory event log. Streams reconnect at
most every 25 seconds to repeat normal membership/session authorization.

React Query owns server data. The browser compares counters, fetches a full view
only for graph changes, and merges pending metadata when the graph revision still
matches. A concurrently changed graph or a new target with an unknown natural or
overlay parent forces a full coherent refresh. Replaced
unreferenced sliding captures are removed from the client cache; referenced and
retained history stays. Hidden pages close subscriptions. Stream errors use a
15-to-120-second bounded fallback that first reads only revision counters. LIVE
expiry uses a scheduled local timer and does not make network requests.
If a rolling deployment reaches an older backend without revision endpoints,
that bounded fallback temporarily refreshes the complete view. Authorization and
transient failures do not trigger this compatibility fallback.

The filesystem development backend persists counters but does not provide
PostgreSQL's cross-file transactional guarantee. Production uses PostgreSQL.

`pending-view` returns only current capture metadata, but its graph still derives
from a coherent complete metadata read. Committed branch inclusion can be reused
by graph/evidence revision and negotiated V2 avoids retransmitting raw timelines.
This endpoint is smaller than a full view; it is not a constant-cost database read.
