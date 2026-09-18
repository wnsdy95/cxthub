# Live capture and repository updates

Live capture preserves the native provider transcript and publishes a per-session
pending pointer. It does not commit context, advance a branch or inject another
session's conversation into an active provider window.

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

- `GET /repos/{repoID}/view`: initial complete graph generation plus revision.
- `GET /repos/{repoID}/pending-view`: pending pointers and snapshot metadata needed
  to connect them to existing shared anchors, from one committed generation.
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
matches. A concurrently changed graph forces a full coherent refresh. Replaced
unreferenced sliding captures are removed from the client cache; referenced and
retained history stays. Hidden pages close subscriptions. Stream errors use a
15-to-120-second bounded fallback that first reads only revision counters. LIVE
expiry uses a scheduled local timer and does not make network requests.
If a rolling deployment reaches an older backend without revision endpoints,
that bounded fallback temporarily refreshes the complete view. Authorization and
transient failures do not trigger this compatibility fallback.

The filesystem development backend persists counters but does not provide
PostgreSQL's cross-file transactional guarantee. Production uses PostgreSQL.
