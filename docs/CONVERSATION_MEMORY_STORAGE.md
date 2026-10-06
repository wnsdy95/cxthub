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

Raw/v1 storage, oversized legacy chunk partitions, and peers without bounded
v2 support use the existing upload path. This fallback is decided before any
upload writes; corruption, cancellation, malformed negotiation, and partial
upload failures are errors, not compatibility signals. Server verification and
repository ownership checks remain mandatory. Memory uses its independent
version/attachment protocol and does not enter this conversation upload path.

Eliminating that remaining full-stream verification requires a separately
versioned manifest-root identity and a durable verified-chunk contract. Such a
protocol must be explicitly negotiated; an old server must never interpret a
manifest-root hash as a legacy whole-document hash. Persisted verification also
needs a defined corruption/invalidation policy and repository ownership checks.

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
