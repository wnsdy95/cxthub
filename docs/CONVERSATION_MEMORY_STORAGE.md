# Conversation chunks and memory versions

Conversation and memory have separate persistence contracts. An archived
conversation is immutable evidence. Memory is a versioned interpretation of
that evidence and may be rewritten when it is compressed or reconciled.

## Conversation

- Persist immutable content-addressed chunks and their ordered manifest.
- Transfer only chunks the destination repository does not own.
- Preserve existing chunks when a provider compacts its active conversation.
  Compaction creates a new event or source generation, not permission to erase
  an earlier archive.
- Bind Git commits to verified conversation ranges. PR integration composes
  those ranges using accepted history; it does not rewrite their source bytes.
- Keep byte-chunk identity distinct from conversation event/range identity.
  A tool result can cross a byte-chunk boundary.

## Memory

- A new compressed summary is a new immutable memory object. Earlier versions
  remain addressable; changing the current attachment does not replace their
  contents.
- Record the source snapshot and previous memory hash. Preserve existing
  selection/history provenance, including code applicability and merged
  sources; a timestamp alone is not proof of coverage.
- Publish a replacement using the observed previous version. A concurrent
  replacement must conflict rather than silently overwrite the winner.
- Memory-only changes must not republish or reindex conversation documents.
- Keep the current memory concise. Do not preserve the complete transcript by
  recursively concatenating old summaries: the immutable archive is the source
  for historical retrieval.

## Compatibility and implementation boundaries

Existing document IDs are SHA-256 of the entire canonical CIR document. V1 and
V2 chunk manifests are physical representations of that same identity. They
must keep their original IDs, natural parents and source bytes.

Chunk-first verification and publication can avoid cumulative byte buffers,
re-chunking and repeated JSON decoding while preserving this identity. They
still have to hash the canonical byte stream and check current chunk integrity.
This is **not** a claim of fully incremental computation or constant-time save.

The CLI's compatible upload path starts from the stored v2 manifest instead of
decoding the full conversation and planning its chunks again. It verifies the
local representation, negotiates missing chunk IDs, loads bounded batches, and
waits for document finalization before publishing snapshot metadata or refs.
An authenticated local receipt can avoid repeated CIR decoding only after every
current stored file has been hashed. The descriptor is captured from those same
verified bytes; requested bodies are hash-checked again when read. A cold check
still reconstructs and validates the complete canonical document.

Receipt v2 adds decoded chunk hashes and sizes without changing canonical/CIR
validation. Ordinary fetch continues to reuse authenticated v1 receipts after
hashing current files; only upload upgrades them through full validation. This
compatibility is specific to the v1-to-v2 metadata change, not future validation
versions. Losing or rejecting a receipt always falls back to full verification.

Raw/v1 storage, partitions exceeding chunk/body/manifest limits, and peers
without bounded v2 support use the existing upload path. This fallback is decided before any
upload writes; corruption, cancellation, malformed negotiation, and partial
upload failures are errors, not compatibility signals. Server verification and
repository ownership checks remain mandatory. Memory uses its independent
version/attachment protocol and does not enter this conversation upload path.

Server history verification also uses bounded, process-local hash-only proofs.
For chunked storage, a proof binds the repository, canonical document ID, exact
stored manifest and every distinct chunk's current stored bytes. FS and
PostgreSQL still read current repository-owned objects on every call. A warm
match avoids chunk decompression and cumulative document assembly; it does not
cache ownership or trust file existence. A cold or changed representation is
assembled from the same captured bytes and checked for canonical identity and
CIR validity. Repacking, rollback, cache eviction or restart never grants trust
to different bytes. Authorization and history writes retain their transaction
boundaries, and memory versioning is unchanged.

Eliminating that remaining full-stream verification requires a separately
versioned manifest-root identity and a durable verified-chunk contract. Such a
protocol must be explicitly negotiated; an old server must never interpret a
manifest-root hash as a legacy whole-document hash. Persisted verification also
needs a defined corruption/invalidation policy and repository ownership checks.

## Metadata acquisition and verified observations

CLI sync keeps two separate checkpoints:

- A **metadata checkpoint** contains snapshot records received from a specific
  repository and credential-free API endpoint. Every acquisition first reads an
  authorized manifest or selected-branch plan. A cache-only response also makes
  an empty pull request: manifest access alone does not grant pull permission.
  Only records with the exact
  current snapshot-state token can be reused; other records are fetched in
  batches of at most 256. Each complete, validated batch becomes an immutable
  metadata page. A small checksummed head lists page hashes in append order and
  advances by compare-and-swap, so a later failure can resume acquisition without
  rewriting all previous metadata. A legacy server without state tokens gets
  fresh reads.
- A **remote observation** is published by the application only after its
  document, attachment and history preflight succeeds. Metadata checkpoints
  cannot supply verified negotiation haves, change the working position, adopt
  snapshot pointers, or move refs. A cache hit still returns every requested
  snapshot to the application's existing validation path.

The checkpoint is a hint cache, not a complete repository view. A complete
manifest retires absent IDs with metadata tombstones, including when the catalog
becomes empty. A selected-branch plan must not evict another branch's records.
Only IDs selected by the fresh server response are returned. Losing an optional
cache insertion race leaves the winning head intact and can finish acquisition
without persisting more metadata. Retirement conflicts, stored corruption,
cancellation, denied access and inconsistent responses remain errors. Read-only
CLI composition does not create this cache.

Reading a checkpoint verifies every referenced page and applies later metadata
for repeated IDs. Compaction replaces a long page list with pages of the latest
records, preserving the CAS boundary. Its threshold grows with the compacted
page count so a large initial catalog does not compact after every new batch.
Unreferenced metadata pages are retained; this cache does not perform archive
garbage collection or establish a server-side Merkle synchronization protocol.

This avoids repeated metadata downloads, including after interrupted content
verification. It does not remove the server's full-manifest scan, local metadata
cache reads, cold document verification or legacy canonical hashing. A durable
server change feed now has an optional [version 1 query contract](CATALOG_SYNC.md)
with committed cursors, scope, repository epochs, tombstones and coherent
pagination. Capable CLI clients consume that journal and reconcile expired
checkpoints through optional Merkle ranges. Legacy clients still use the full
manifest. See the catalog contract for separate acquisition/verification limits.
The existing graph/pending revision counters intentionally exclude staged
objects and therefore cannot serve as a complete catalog cursor.

## Required validation

1. Appending conversation changes only the new/tail chunks; old archives remain
   readable and their IDs stay stable.
2. A memory-only replacement changes no conversation object or search index and
   keeps earlier memory versions readable.
3. Two replacements from the same previous memory cannot both win.
4. Changed, missing, unowned, reordered or malformed chunks cannot produce a
   valid publication. Cancellation and expired job leases publish nothing.
5. Native compaction and independent branches preserve their original sources.
6. Measure newly transferred bytes, bytes read/hashed, parsing, allocations,
   indexing and completion wait separately. Reduced allocation is not proof of
   reduced cold indexing time or end-to-end transfer latency.
