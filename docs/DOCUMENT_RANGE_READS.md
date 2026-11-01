# Conversation range reads

## Two different integrity contracts

The archive and the viewer are separate contracts. Chunked storage alone does not
make a page query proportional to its selected range.

| Request | Verification | Intended consumer |
| --- | --- | --- |
| `GET .../docs/{hash}/events` without `view` | Existing strict root verification, including every current chunk | Compatibility and full dependency audit |
| `GET .../docs/{hash}/events?view=chat` | Repository ownership, live verified generation binding, authenticated selected index paths/pages and selected chunk hashes; FS retains current root/header verification | Conversation viewer |
| `GET .../docs/{hash}/event-fragments?index=N` | Same range proof, reading only the selected bytes | Explicit expansion of oversized events |

A successful range read does **not** certify unselected chunks. A successful
strict read must still reject damage outside the visible page. Neither endpoint
rewrites or truncates the original archive. Response metadata (`event_index`,
`event_bytes`, `deferred`) is not part of the stored CIR or its identity.
Existing MCP/CLI canonical archive readers retain their existing strict contracts;
this change does not silently replace their full dependency verification with the
viewer projection.

## Publication and older roots

The v1 range projection contains a manifest-bound header and hash-checked pages of
128 event descriptors. PostgreSQL additionally publishes a compact v2 generation
with hierarchical directories; the viewer uses that generation without loading
the complete v1 header. Event descriptors carry canonical byte locations, event hashes,
sequence, kind, role and the existing compaction flag. They contain no tool output
or conversation body.

- Root publication requires complete current dependency verification.
- PostgreSQL commits the root grant, projection and successful finalization job
  in the existing publication transaction. Failed publication rolls back all of them.
- FS writes pages before the header and completed job receipt. Individual files
  are atomically replaced; this is not a multi-file database transaction. Interrupted
  publication remains retryable, and an absent header is never treated as proof.
- A missing projection on an older PostgreSQL root is durably queued by the
  viewer and built after complete streaming verification under the bulk-work
  limiter. The viewer returns `doc_range_index_pending` (503, `Retry-After: 3`)
  without building the index inside that request. The API process runs the worker;
  the MCP process does not. FS compatibility retains synchronous bounded work.
  The archive, refs, snapshot IDs and activity revision do not change.
- Cold verification and root-job preparation spool descriptor pages to an
  adapter-owned temporary file. Root jobs validate the entire event stream one
  chunk at a time without retaining an owned copy of the complete conversation.
  Compatibility publication from an already-owned proof regenerates one
  descriptor page at a time. None of these paths retains all descriptor bodies
  in memory. The 32 MiB limit
  bounds the header, not the sum of pages: large valid archives containing many
  short events are not rejected merely because the derived pages exceed 32 MiB.
- A prepared root job is bound to its repository, job identity, claim version
  and canonical manifest. Completion rechecks the current policy and lease,
  and rehashes every current repository-owned chunk while retaining the source
  rows through the PostgreSQL commit. Preparation cannot grant access, reuse a
  foreign repository's ownership or reconstruct a missing chunk. The temporary
  spool is closed on success, preparation/completion failure, cancellation and
  read-transaction commit failure. Legacy document jobs keep their existing
  compatibility path.
- A present but invalid projection fails closed. It is not silently repaired by
  a viewer request.
- Existing legacy-identity documents retain their verified legacy read-index
  path. Their old indexes lack event-kind metadata, so classification can still
  read event bodies. The constant selected-chunk guarantee applies to indexed
  manifest-identity roots, not to every historical storage format.

PostgreSQL projections are globally deduplicated by document hash, but every
descriptor/page/body read still requires that repository's ownership grant. A cache
or shared derivative is never an authorization grant.

### Compact PostgreSQL generations (local implementation, 2026-10-19)

`0095_doc_range_generation.sql` adds a compact descriptor and document-scoped,
hash-addressed derived objects. Separate fanout-128 directories authenticate event
pages and pages of 128 chunk descriptors. Opening a viewer reads one bounded
descriptor, not the root bytes, manifest chunk list or all event-page hashes.
Selected metadata walks are logarithmic in the page count; each fetched page has
a fixed byte/count limit. Per-read caches are bounded. Body reads keep their
existing byte budget and current chunk ownership/hash checks.

The generation is a publication receipt, not a self-authenticating replacement
document identity. The publisher accepts only an opaque fully verified range
proof while retaining current source and ownership rows through the transaction.
Descriptor, directories, event pages and job completion become visible atomically.
The original document hash and bytes remain unchanged. A descriptor checksum
alone would not prove that its mapping came from that document.
The compatibility increment retains v1 event pages alongside v2 objects, so it
adds derived metadata storage/writes, not transcript copies. It does not yet
shorten publication transactions; write-cost reduction remains a separate D4 gate.

Every blob-row update advances a trigger-controlled monotonic source version in
the same transaction, including attempts to reset just that version field.
Publication records that version while retaining the verified source lock; open,
readiness and completed-job reuse require the current source version to match.
An UPDATE trigger also deletes that root's v2 generation and derived objects,
but deletion alone is insufficient: an older writable REPEATABLE READ snapshot
may not see a subsequently published generation. The version fence rejects any
such retained stale row. DELETE cascades through restrictive root/generation
foreign keys. Even a no-op byte update advances the version, without comparing
large values or blocking legacy re-encoding. The v1 index and authoritative
content identities are unaffected. Selected chunk updates are caught by
current-byte checks; unselected corruption remains the strict audit's concern.
This relies on the publisher and database constraints, not protection against
privileged coordinated rewriting of the authoritative binding and all hashes.

The optional `DocRangeGenerationStore` port accepts only its own read-only
REPEATABLE READ scope. It verifies current repository ownership, root existence,
the descriptor commitment and requested identity together; selected paths and
pages are verified within that same scope. Returned descriptors are owned copies.
An old pinned snapshot may finish coherently; fresh snapshots observe invalidation.
Closed, foreign or writable transactions cannot reopen through the pool.

Readiness and completed-job reuse now require a source-version-matched v2 generation. A v1-only document
is queued for complete current-source verification, not upgraded by trusting its
persisted v1 metadata. Missing or source-version-mismatched v2 means unavailable/pending; malformed current v2
fails closed without a repair or full-header fallback during the request.

### Persisted exact inherited boundaries (local implementation, 2026-10-19)

For PostgreSQL manifest-identity pairs, the viewer uses a separately persisted
exact-prefix result. The key contains repository, selected document, base
document and comparison algorithm. It also binds both current root source
versions and both validated generation descriptor hashes. The request checks
both documents' current ownership and snapshot identity, even for a completed
boundary. A different generation or source version cannot reuse old progress.

A missing result ends the owned read transaction and releases its interactive
permit before queueing. The existing durable range queue distinguishes a
document-index job from a document/base comparison job; both share the maximum
32 active jobs and single running job per repository. The API returns the same
503 `doc_range_index_pending`/`Retry-After: 3` contract. Explicit read-only callers
do not enqueue. Borrowed transactions cannot escape to a separate write through
the pool. Pages and event fragments share a preparation coordinator with at most
two actual reads. An already completed receipt permits a fresh read, not trust
in the receipt as a content proof. If a generation disappears between readiness
and opening, or a pair becomes stale after its completed receipt, release the
read and repair the current sources again. Root pairs also require a new boundary
enqueue/validated receipt, including when a concurrent worker has already rebuilt
the generation. Exhaustion returns Pending only after this recovery handoff;
it does not leak a terminal unavailable sentinel or an empty successful page.
Integrity, ownership, unrelated unavailable errors and explicit read-only failures
are not treated as these preparation races.

Foreground enqueue uses a 500ms context budget covering pool acquisition, the
transaction and its rollback, plus a 250ms per-lock PostgreSQL timeout. A timed-out
lock or internal enqueue deadline returns `resource_overloaded`, not a fictitious
Pending receipt. Parent cancellation/deadlines remain distinct, and cleanup
cannot turn an established integrity/not-found error into overload. An existing
receipt is checked before acquiring source/advisory locks. Worker verification,
source-lock lifetimes and publication fences are not shortened by this bound.
HTTP views and fragments carry one request/transport deadline across preparation
and retries, without an outer permit that would prevent releasing capacity.

Each comparison batch examines at most 64 event pages (8,192 original events).
Matching authenticated page hashes skip per-event reads; unequal pages compare
exact canonical event hashes until the first difference. This is not semantic
deduplication. Comparison opens a coherent read snapshot, then closes it before
the short write transaction. Publication locks both roots and ownership rows in
stable order, rechecks source versions, generation bindings and the live job
lease/version, and commits the checkpoint with its waiting/completed state.
A stale worker or changed source cannot advance the checkpoint.

Incomplete checkpoints are internal worker state, never a partial `inherited`
answer. The next claim resumes at its verified page boundary; service restarts
do not restart a valid prefix from zero. When a binding changes, the old
checkpoint is discarded for comparison and the new generation starts from zero.
Completed reads load the small bindings and boundary record, not every prefix
page. Identical document/base IDs need no comparison job after scoped validation.

Source transcripts, snapshots, refs, memory and activity revisions are not
rewritten by this work. Reusing a prefix is not an audit of the omitted current
chunk bytes. Strict archive verification and FS/legacy comparison retain their
existing contracts. Visible-message filtering still has the 512-event scan
ceiling; persisting an inherited boundary alone does not fix tool-heavy paging.

This migration changes the range-job identity from `(repo, doc)` to
`(repo, doc, base)`; an empty base retains document-index work. Drain old queue
workers before migrating and start only compatible API workers afterward.
An older binary's enqueue conflict target is not compatible with this schema.
Do not treat a binary-only downgrade as a validated rollback. The D5 rollout
and reader-path rollback gates remain open; no production migration is implied
by the local tests.
The public v1 reader and FS contract remain available for compatibility/audit.
HTTP success bodies and document IDs do not change. Persistent inherited
boundaries and the separately negotiated visible-unit contract are implemented
locally; release verification remains unfinished D5 work.

Rollout must install the migration first and stop/drain any old range-backfill
worker before enabling v2-serving instances. An old v1-only worker could consume
the same job and mark it completed without producing v2. New readiness checks
would prevent false success, but repeated reclamation could delay availability.
Mixed worker versions are not a supported rolling-upgrade contract; a target
column alone would not exclude workers that do not understand it. Rollback must
keep the migration's lifetime guards and switch readers/workers together.

### Transaction-scoped v1 header reuse (preceding D2a checkpoint)

The optional `DocRangeSnapshotStore` port opens an adapter-owned reader in a
positively identified read-only PostgreSQL snapshot. Opening verifies the current
root and full index header once. Each page then selects only its descriptor bytes,
checks repository ownership in that same snapshot, and verifies the page hash
against the privately retained header. Returned header slices are defensive
copies, not a caller-controlled proof. The reader requires its originating
transaction context and cannot reopen a closed transaction through the pool.

At the D2b checkpoint, PostgreSQL preparation used a constant-width owned-document/index-existence
probe instead of fetching the complete root/header a second time. Existence is
not a validation receipt: the actual read still verifies the current root/header,
pages and selected bytes. A present corrupt header is never silently rebuilt.

The page and header therefore describe the same MVCC generation. A later request
rechecks the current root/header/ownership and observes committed damage or
revocation; the reader is not a cross-request validation cache. FS, unknown and
write transactions retain the existing per-page revalidation behavior. Selected
content bytes are still independently hash-checked, and strict archive readers
still inspect all required chunks.

This removed repeated header SQL/validation within a v1 read. The preparation
check remains but no longer transfers the complete header in PostgreSQL. It
does not bound the first validated header read or create a persistent
inherited-boundary cache. Those are separate stages of the
[incremental read plan](INCREMENTAL_READ_PUBLICATION_PLAN.md).

### Durable missing-index preparation (local implementation, 2026-10-18)

`doc_range_jobs_v1` deduplicates requests by repository and document hash. A short
enqueue transaction accepts only an existing repository-owned RootV1 snapshot
whose ID matches the document hash. It stores no transcript or manifest. There
can be at most 32 waiting/running/retrying jobs per repository and one running
job per repository. Queue capacity returns the existing bounded overload error.
Existing owned receipts are read without source-row locks. A cold miss is
rechecked after enqueue serialization and before source pinning, so concurrent
enqueue/build cannot turn a pending poll into a wait for full verification.

The API worker obtains bulk admission before claiming a job. Claims are leased
and versioned; renewal, failure recording and publication reject stale or
expired claims. Complete current-byte verification pins the source rows while
building. The index and completion receipt commit together, without changing
source content, refs or repository activity revision. A crashed process leaves
a durable claim that another process can recover after lease expiry. This is
restartable work, not persisted progress inside a partially built index: a new
claim verifies the document from the beginning.
Canceled or timed-out verification stays retryable even when wrapped in a
manifest-validation error. A genuine invalid or no-longer-owned source is
terminal and is never silently repaired by the viewer.

The browser retries only the explicit preparation response in document-page and
event-detail queries. At most nine automatic retries use bounded exponential
backoff, jitter and the server hint, with at most 90 seconds of waiting in total
(network request time is additional). Errors then retain the existing manual
retry. Graph retries are unchanged; corruption, missing sources, denied access
and `doc_range_index_unavailable` are not preparation retries. Read-only callers
cannot enqueue work or escape into a writable pool connection.

Roll out migrations `0093_doc_range_index.sql` and `0094_doc_range_jobs.sql`
and the API before the new viewer.
The old no-`view` endpoint remains compatible. Do not assume an older server that
ignores `view` implements range reads. These tables are derived indexes, not new
conversation IDs, branch edges or a migration of the original archive. No
production migration, deployment or resource-plan change is performed by the
local regression script.

## Visible-unit index foundation (local implementation, 2026-10-19)

Sealed range metadata now includes bounded seed-summary/empty-reasoning
classification and cumulative rendered ranks. Derived metadata changes neither
canonical event bytes nor document identity. Old metadata remains decodable,
but absent flags/ranks are not treated as a verified presentation proof.

The `DocVisibleGeneration` projection stores mode-specific unit pages and
authenticated directories. Its builder takes a sealed generation/index binding,
not a caller-supplied descriptor hash. Units preserve original ordinal coverage,
optional anchors, originating prompt indices and folded counts, including leading work,
empty reasoning and unanswered tails. Source bodies are not copied into the
index. Its bounded paths seek either a raw event or a display unit without
walking every hidden descriptor. Supplied clipping ranks are trusted only when
obtained from the matching verified range metadata; structural validity alone
does not authenticate a rank.

Migration 0097 binds the visible descriptor to the current root source version
and v2 generation hash. Publication uses the same transaction as range metadata
and the worker receipt. Root jobs build both projections, while explicit visible
preparation can upgrade an existing range-only completed job without creating a
second queue or bypassing capacity limits. Current corrupt metadata is not an
automatic missing-index rebuild. The reader accepts only its originating
read-only REPEATABLE READ transaction, preserves current source/ownership checks
on subsequent requests and validates selected objects within fixed size bounds.

The local implementation now exposes the explicitly negotiated `visible-v1`
HTTP capability below. Existing event pages, fragment reads and strict audits
retain their contracts. Web uses a separate unit/fold validator rather than
passing sparse anchors to the old contiguous-event renderer.
Keep incompatible old/new backfill workers drained during migration/cutover;
the full release/rollback rehearsal remains a D5 gate, not local test evidence.

Old range generations may omit the new ranks/classification. Their existing
contiguous reads remain valid, but visible preparation must verify the source
and publish a new generation. That descriptor change invalidates dependent
prefix receipts even though source identity/version is unchanged. Prefix work
is requeued against the new pair and must recover the same exact inherited
ordinal; the viewer must not skip readiness because the root job just completed.

## Explicit unit/fold pagination (`visible-v1`)

The existing events route accepts these mutually exclusive shapes:

```text
?projection=visible-v1&view=chat&offset=-1&limit=20&base=<hash>
?projection=visible-v1&cursor=<selection>
?projection=visible-v1&expand=<fold-selection>
```

Initial `view` is required (`all`, `chat`, `prompts`); offset defaults to -1,
limit to 20, maximum 100. Initial base is optional. Cursor/expand requests must
not repeat initial parameters. Unknown, duplicate, empty or malformed parameters
are rejected; cursor purpose and route scope are checked before querying data.

Responses identify projection, kind (`units` or `fold`), hash, base, generation,
rules, view, total, inherited prefix and selection start. `from`/`to` are original
half-open archive ordinals. `next_cursor: null` is terminal. Unit anchors carry
their original `event_index` and explicit `seed_summary`/`empty_reasoning` flags;
the optional fold and anchor partition the unit without reordering its contents.
The whole selection remains bound on every continuation, including the base.

The binding includes the range generation's lifetime UUID, current source version
and authenticated descriptor. Migration `0098_doc_read_source_incarnation.sql`
adds the UUID to the small derived generation table, not `blobs`. A deleted and
rebuilt generation receives a new UUID, even if source bytes and the descriptor
hash are unchanged; ordinary metadata updates preserve it. This prevents an old
cursor becoming valid again after delete/reinsert resets a source counter. The
selected and base lifetimes both participate. Conversation IDs remain unchanged.

The unsigned base64url cursor is a bounded canonical selection, not a capability
or proof of previous issuance. Valid editing may choose another authorized
location. The server opens the current owned generation, derives the selected
unit and clipped fold bounds, and checks all claimed scope/counts/positions.
It does not trust a cursor-supplied descriptor, rank or fold end. The browser
independently rejects changed selection context, skipped or overlapping ranges,
wrong kinds and repeated continuation tokens before appending anything.

After each admission wait, visible reads reauthorize before opening a read
snapshot or enqueueing preparation. Credentials, public visibility, repository
role and break-glass policy reuse the HTTP role gate. Trusted internal callers
must explicitly identify a system actor; an absent authorizer does not silently
grant access. Break-glass audit work stays outside the read-only transaction.
Every page then checks current document/base ownership and pinned generation
bindings, and verifies selected metadata and source chunks.

Main pages cap unit count and serialized payload (128 KiB). Fold pages cap raw
events at 128 and use the same byte budget; fold pagination never appends hidden
work to the main stream. Tools, reasoning, summaries and oversized anchors stay
deferred. Opening a fold fetches bounded metadata plus ordinary messages, not
one raw-detail request per event. Raw/fragment detail remains explicitly selected.
These are upper bounds, not a guarantee that a byte-heavy or empty page contains
exactly one viewport of rendered text.

Unsupported legacy/FS projections use explicit `doc_visible_projection_unsupported`
(409), allowing a whole-stream compatibility fallback. Auth failures, corrupt
metadata, stale selections and pending indexes must not trigger that fallback.
Unavailable current metadata follows the existing bounded durable preparation
contract (503), never a foreground full verification. After rebuilding, an old
generation returns `doc_visible_generation_stale` (409), requiring restart;
neither the API nor Web silently splices a replacement generation into a stream.

Web keeps one bottom sentinel for main pages and local pagination inside an
explicitly opened fold. Empty units render no placeholder spacing. Automatic
advancement across zero-row pages stops after two pages per burst and exposes
only bottom continuation, so a long empty-reasoning history cannot trigger an
unbounded request chain. Shared cursor-history nodes keep retained history linear
in loaded pages. Restart isolates the complete selection and its fold caches.

Local verification includes synthetic PostgreSQL output through the actual Web
validators and separate browser fixture regressions. It does not certify deployed
mixed-load behavior. Drain old workers and migrate through 0098 before enabling
the new reader/worker pair; a reader fallback does not make an incompatible old
queue writer safe. Original transcript data is not rewritten during this rollout.

## Compatibility screenful pagination

The compatibility viewer opts into `view=chat`, `prompts` or `all`. Its default batch aims for
20 visible items, with a 128 KiB soft payload budget and a 512-event traversal
ceiling. `next` always denotes the next absolute archive ordinal, not a count of
visible messages. Tool-heavy spans can contain metadata placeholders and fewer
visible messages when the traversal/byte ceiling is reached.
Deferred placeholders consume their actual serialized size, not a fixed charge
that prematurely ends a mostly-hidden page. Prompt-only spans can still contain
no visible prompt. The browser bounds hidden-only automatic scanning and offers
a continuation at the bottom rather than repeatedly scanning an entire archive
without user interaction. Twenty visible items is a target, not an unconditional
promise that can override byte and traversal limits.

Hidden tools and reasoning do not ship their bodies with the page. Expansion
requests `view=raw&offset=N&limit=1`. Events exceeding 128 KiB stay deferred even
in raw view; explicit fragment navigation reads up to 64 KiB of UTF-8 per request.
The browser does not automatically reconstruct a potentially hundreds-of-MiB
event in memory. Full source remains available through the existing archive path.

The viewport prefetches one following batch near its bottom. There is only one
pagination boundary per document; later documents mount after earlier pages are
exhausted. Mode, repository, document, base and start offset identify separate
query-cache entries. Failed reads require a bounded/manual retry rather than an
intersection-triggered retry loop.

Inherited prefixes compare exact event hashes; equal range-index blocks can be
skipped without loading bodies. Pagination keeps original ordinals for detail
requests and turn grouping, including after a mode change.
The browser sends `base` only for the initial page of a query, including retries
of that page and explicit expansion from offset zero. It retains that page's
server-computed `inherited` boundary for the entire stream. Following pages use
the server's absolute `next` cursor without `base`, so a legacy parent is not
reopened and compared on every scroll. Mode/base/start changes create a separate
query and compute its initial boundary again; authorization and selected-page
integrity checks still run on every request.

## Independent pending reads and shared admission

On Hold requests opt into `pending-view?projection=on-hold-v1`. This response has
an explicit projection tag and **no `graph` field**. It shares publication and
reachability classification with the full graph, but skips code-evidence and
branch-integration construction. Its separate query cache cannot overwrite the
complete graph. The original full pending-view contract remains available.

Heavy archive reads, uploads, graph work and workers share process-level
admission. Capacity is bounded rather than allowing concurrent archives to consume
unlimited memory; queue saturation returns retryable 503. Revision notifications
remain lightweight. Completed graph projections are bounded and keyed by coherent
revision plus actual inputs, repository and selected position; they are not reused
as authorization or across uncommitted transactions.

Projection cache entries use compressed JSON within the same 8 MiB total retained
budget and four-entry limit. Expansion is independently capped at 32 MiB, and
reads validate the compressed stream through its checksum and end before returning
an isolated graph value. Projections beyond either budget remain valid results but
are not cached. This avoids silently disabling reuse for repeated identifier-heavy
graphs: the incident corpus was 4.54 MB on the indexed wire, but 15.68 MB as the
expanded projection and therefore never fit the former raw 8 MiB cache. Warm
projection reads do not acquire the bulk worker slot; cold graph computation and
metadata reads still have their existing resource and consistency requirements.

PostgreSQL graph inputs use stable ordering, including snapshot-ID tie-breaking
for equal creation timestamps. Ref, pending and unsynced-pointer row placement
must not change an otherwise identical input fingerprint after an MVCC update.
Repository revisions, coherent-read fingerprints and authorization checks remain
part of every cache lookup; ordering is not permission to serve stale results.

When a bulk slot becomes available, its queued work takes precedence over queued
interactive work. At most one of the two active slots can be bulk, leaving the
other available for interactive requests. Always prioritizing interactive reads
can starve queued uploads and verification when cold graph reads repeatedly fail
their bulk upgrade and retry. Both class reservations have deterministic tests;
wait deadlines and capacity limits are unchanged.

This does not make all metadata reads constant-cost: pending classification still
walks repository metadata, and graph-cache lookup still fingerprints coherent
inputs. Admission controls concurrent work, not the working-set size of a single
archive or memory retained by callers after they receive a result.

## Verification gates

The following regressions are required before release:

- Grow a root from 80 to 800 hidden tool events: the same first 20 visible
  messages must still read one content chunk and one index page, not the archive.
- Damage an unselected chunk: range read succeeds but strict audit fails.
- Damage a selected chunk/index page or remove repository ownership: range read fails.
- Publication failure/cancellation must not expose an unverified projection.
- Equal inherited blocks must not read their event bodies.
- Tool-heavy chat/prompts pages count visible messages; raw expansion returns the
  original event; UTF-8 fragments reconstruct the original bytes exactly.
- Browser tests cover prefetch, mode changes, lazy bodies, errors, and independent
  pending-session display while the full graph is unavailable.
- An opt-in synthetic PostgreSQL load runs server reads, uploads and a real
  finalization worker in a Docker process capped at 512 MiB. Seed generation must
  run separately so the client fixture is not confused with server memory.

Local constrained-load results are not proof that every production overlap is
safe. Corpus sizes, worker size, memory-limit settings, rejected requests, peak
memory and latency must accompany any reported result. Deployment and production
OOM verification remain separate steps.

## Initial constrained-load measurements (2026-10-14)

`scripts/test-doc-range-load.sh` creates a disposable local PostgreSQL database
and a Linux application container capped at 512 MiB with swap disabled. The
fixture has a 310 MiB manifest-root conversation, 83,146 events (mostly tools),
and 700 linear snapshots. PostgreSQL and fixture generation run outside the
application cgroup. Bounded HTTP clients run inside it. This is synthetic data,
not the production corpus or its PR/merge topology.

| Scenario | Go soft target | Cgroup peak | Initial result |
| --- | --- | --- | --- |
| Pages, graph, pending, uploads and 32 MiB finalization | 400 MiB | 256.84 MiB | Pass; 64 retryable 503 responses |
| Same reads and 310 MiB finalization | 400 MiB | 512.31 MiB | Fails strict peak gate; 570 retryable 503 responses |
| Same reads and 32 MiB finalization | Disabled | 512.20 MiB | Fails strict peak gate; 61 retryable 503 responses |
| Isolated range page and strict audit | Disabled | 104.05 MiB | Pass |

All workers and response/checksum checks completed and no OOM was recorded in
these runs. Kernel-accounted peaks slightly above the cap are nevertheless
reported as failures, not silently rounded down. The current production runtime
does not acquire a Go soft memory target from these tests. A soft target also
does not replace the container's hard limit or guarantee that a large live heap
will fit.

The isolated first-page sample was 25.2 ms for `view=chat`, versus 4.423 seconds
for the strict endpoint. These are different integrity/payload contracts, one
sample each in chat-first order, not equivalent cold-cache benchmarks or an SLA.
Initial runs used publication-created indexes; lazy backfill was not measured
in them. Larger working sets, cold indexes and production-shaped graphs require
separate evidence before claiming general 512 MiB safety.

## Final streaming-publication comparison

The initial publication path still held the whole verified document in memory,
even after descriptor-page spooling. Root-job preparation now uses the same
streaming semantic verifier as cold range-index creation, with current dependency
hashes, ownership, policy and lease rechecked in the final publication transaction.

The controlled comparison feeds upload clients from a disposable fixture table
in PostgreSQL, one hash-checked HTTP payload at a time (at most 1 MiB). The same
310 MiB document is still uploaded through the real HTTP API and finalized by
the real worker. This avoids charging the application cgroup for a second,
artificial 310 MiB client fixture-file cache. The original file-input results
remain failures; neither the hard limit nor the strict peak gate was relaxed.

| Root-job path | Go soft target | Peak | Result |
| --- | --- | --- | --- |
| Owned whole-document proof | 400 MiB | 510.01 MiB | Pass, but only 1.99 MiB headroom |
| Owned whole-document proof | 350 MiB | 512.09 MiB | Strict peak failure; no OOM |
| Streaming proof and spooled index | 400 MiB | 248.20 MiB | Pass; 263.80 MiB headroom, no OOM or cgroup pressure event |
| Streaming proof and spooled index | Disabled | 512.004 MiB | OOM-killed; incomplete workload, not a successful latency sample |

The successful final mixed run took 20.91 seconds. All original-body/hash checks
and the durable completion receipt passed. Its 1,286 HTTP attempts included 213
retryable 503 responses; overload is bounded rather than hidden or counted as
uninterrupted availability. No rejection rate or latency SLA is claimed.
The API and MCP Dockerfiles now default to `GOMEMLIMIT=400MiB`, and image preflight
checks that setting. It remains overridable; a different value or workload needs
fresh validation. These local edits do not update already-running deployments.

Kernel cgroup accounting includes file cache, and `memory.max` can briefly be
exceeded; `memory.events.max` and an OOM kill are distinct. Our regression still
requires a peak at or below 512 MiB in addition to zero OOM kills.
See the [kernel memory-controller contract](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html#memory-interface-files).
Go's soft target is not a substitute for the hard limit. A single oversized event,
strict legacy reads, other background workloads and real multi-branch topology
still need separate capacity evidence; this gate is not universal OOM immunity.

Reproduce only on local disposable resources:

```sh
CXT_RANGE_UPLOAD_SOURCE=postgres CXT_RANGE_WORKER_MIB=310 \
CXT_RANGE_GOMEMLIMIT=400MiB bash scripts/test-doc-range-load.sh
```

Use `CXT_RANGE_CASE=cold` for the missing-index rebuild scenario. The separate
cold tests peaked below 146 MiB with both the soft target enabled and disabled.
Use `CXT_RANGE_UPLOAD_SOURCE=files` to reproduce the original client-file-cache
stress case. The script retains source, binary hashes and per-run measurements
locally and removes its own test containers/database/network.

## Real-corpus check

The first production rollout on 2026-10-15 still recorded an API OOM at 09:52 UTC
under the 512 MiB limit. A complete graph returned 200 in 12.3 seconds, while other
attempts returned admission overload (503). A small range page returned 200 in
0.39 seconds, but an older large document's cold page did not respond within a
150-second diagnostic client deadline. These are release observations, not an
accepted performance gate. The responsible overlapping workload remains
unidentified in that first rollout. The subsequent investigation and legacy
publication correction are tracked in issue #516.

Admitted HTTP reads and uploads have a two-minute request context and socket
read/write deadline, including queue time. An earlier caller deadline wins.
The permit stays held until the handler actually exits, including response
encoding; cancellation does not prematurely release capacity while work is still
running. Deadlines remain in place through HTTP response finalization and are
reset by `net/http` for the next request. SSE retains its separate streaming
lifetime and write deadlines. Network tests cover stalled HTTP/1.1 and HTTP/2
bodies/responses, permit recovery, and reuse of a healthy keep-alive connection.

An opt-in test copies a specified CLI manifest root and its referenced compressed
chunks to a temporary FS store. It verifies the copied archive before building
the index and checks that the original compressed source files are unchanged.
It prints counts/timings only, not conversation content:

```sh
cd backend
CXT_RANGE_CORPUS_DIR=/absolute/path/to/repository/.cxt \
CXT_RANGE_CORPUS_HASH=sha256:<full-document-hash> \
go test ./internal/app -run '^TestDocViewLocalCorpus$' -v -count=1
```

The local corpus measured on 2026-10-14 contains 309,851,925 bytes, 87,449 events,
591 chunks and 52,806 tool events. Its largest event is a 410,643-byte tool result.
Cold index creation took 11.26 seconds. Twenty-four subsequent pages across the
beginning, middle and tail in three modes took 9–27 ms, read zero to four content
chunks and one to five descriptor pages each, and performed no full-document
reads. Most non-boundary chat pages reached 20 visible messages; the initial
large-text page held eight and the final page fifteen. Prompt-only scans can hit
the traversal ceiling before finding twenty prompts, including a hidden-only
page. These are local FS measurements, not cloud network latencies or a cgroup
memory result.

This check caught a deferred compaction serialization error missed by synthetic
tool-only fixtures. Metadata-only compaction rows now serialize through their
own presentation DTO rather than the archive union that requires replacement or
locked-state payloads; an explicit regression preserves that distinction.

## Legacy finalization incident

Read-only production diagnostics on 2026-10-15 identified a repeatedly reclaimed
legacy `cxt-doc-chunks-v2` finalization: 514 distinct chunks, 107.8 MB compressed,
and more than 277 attempts. New manifest-root jobs were waiting behind it. This
is not evidence of missing migrated database rows: the old worker retained the
entire decoded stream, a second normalized chunk plan, and document-wide search
text. The root-only streaming correction did not cover this legacy path.

Legacy v2 jobs now verify one chunk/event at a time into a private temporary spool.
The original legacy DocHash and exact canonical bytes remain unchanged. Only
event coordinates and hashes survive preparation. Search text is spooled per
event and inserted in globally hash-ordered batches of at most 4 MiB/1,024 rows;
one oversized entry is processed separately, never truncated. Reference-only
checks also avoid retaining every compressed and decoded chunk simultaneously.
PostgreSQL prefetch selects a byte-bounded window before transferring bodies,
rather than treating sixteen arbitrarily large blobs as a bounded batch.

Rebuildable search blocks are staged in short transactions with generation pins.
The worker stops its asynchronous renewal before staging; staging renews between
batches, so its own job-row lock cannot cause a competing renewal to cancel it.
Final publication rechecks the current lease, policy, owned input bytes, private
spool hashes and stored normalized chunks, and commits ownership, the read index
and completion receipt atomically. Existing valid representations and their
other owners are preserved. An existing broken archive is rejected, not silently
repaired from another partitioning of the upload.

Spools are removed on success, rejection, cancellation and ordinary errors.
Memory scratch is limited to 32 MiB per legacy event/decoded input chunk. Larger
valid items report retryable `resource_limit`, not `invalid_document`. Existing
whole/v1 alternate representations are verified only within a 32 MiB compatibility
budget. Those limits do not silently truncate an archive or delete its queue item.

`TestLegacyStreamingPGMemoryQualification` is an opt-in PostgreSQL regression
(`CXT_LEGACY_MEMORY_QUALIFICATION=1`, `CXT_TEST_DSN` pointing to a disposable DB).
It constructs approximately 257 MiB/515 chunks incrementally, publishes the full
index, verifies the stored reference, and keeps another 64 MiB live throughout.
Run the compiled test inside a 512 MiB, swap-disabled container with one CPU,
`GOMAXPROCS=2` and the production `GOMEMLIMIT=400MiB`. It gates sampled heap at
320 MiB; capture cgroup `memory.events` and peak separately. A no-OOM run is not
the stricter mixed HTTP/upload peak gate above, nor proof of universal capacity.
Production loading, queue progress and the OOM timeline still require verification
after deployment. No paid instance upgrade or production queue deletion is part
of this correction.

## Queue-only writes and transient refreshes

Accepting or replaying a PR promotion request stores a durable job and schedules
Git scans; it does not publish graph facts. Both direct submission and GitHub
reconciliation use a revision-neutral repository transaction. Authorization,
binding checks and audit remain transactional. Actual promotion still advances
the graph revision, and published scan evidence advances the evidence revision.
Keeping these separate prevents periodic reconciliation from discarding a warm
graph for every already-known PR. Exact revision, input fingerprint and origin
checks still govern the graph cache; no epoch is ignored to obtain a cache hit.

Unsync cleanup advances the graph revision only when a pointer was actually
removed, including legacy filesystem filenames. An already-shared target cannot
revive an unsync pointer. A real write or removal still advances the revision in
the same repository transaction; PostgreSQL rollback restores both together.

The web client retains the last complete repository generation when a refresh
fails with a narrowly classified transport, gateway or resource-capacity error.
It shows an explicit stale warning and Retry. It does not combine new partial
graph data with old inputs. Initial failures have no cached graph to display;
authorization, integrity and response-contract failures remain blocking, as do
invalid graph structure and unmatched code-position revisions.

These changes avoid unnecessary rebuilds and blanking a previously loaded graph.
They do not remove cold projection cost or promise that a cold read can execute
while another bulk operation occupies the memory budget. Production verification
must distinguish response-header time from transfer time and use the browser's
gzip negotiation; a successful health check alone does not verify graph loading.

Read-only graph, pending, document page and expanded-event queries automatically
retry only an explicit `503 resource_overloaded` response, at most four times.
They use 1/2/4/8-second exponential backoff with up to 250 ms jitter and honor
`Retry-After` when it asks for a longer delay. A server hint over ten seconds is
left to explicit user retry instead of creating an extended automatic wait.
Configured cross-origin browsers can read this response header; no new origin
or credential permission is granted. Query cancellation stops delayed retries
after navigation. Mutations, authentication, authorization, integrity, malformed
responses and untyped gateway failures are not automatically replayed. Persistent
capacity exhaustion still shows the final error or stale-graph warning; bounded
recovery is not a claim that cold generation or capacity limits have disappeared.
