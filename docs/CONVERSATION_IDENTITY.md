# Staged conversation identity contract

Status: domain and metadata preparation. Root publication, reader negotiation
and capture are **not enabled**. Existing CLI/API/MCP paths continue using the
canonical-CIR hash. An explicit root declaration at the current publication
boundary returns `unsupported_document_identity`; it is never silently treated
as a legacy hash.

## Identity and representation

The absent or empty `identity` on a document, and `doc_identity` on a snapshot,
mean the existing SHA-256 of canonical CIR bytes. These fields are omitted for
legacy objects, preserving their wire bytes and snapshot-state fingerprints.
`Snapshot.ID == Snapshot.DocHash` remains required. Identity metadata is
immutable; replay cannot relabel a stored snapshot. Memory and graph overlays
remain separate from the conversation identity.

The staged `cxt-manifest-sha256-v1` scheme hashes the exact bytes:

```
"cxt-conversation-root-v1\0" || canonical_manifest
```

Its token still uses the `sha256:<hex>` syntax. Consumers must compare the
scheme as well as the token; syntax alone cannot identify a root. A manifest
hash is not evidence of valid conversation bodies or repository ownership.

The version-1 manifest contains:

- `version: 1`, `identity: "cxt-manifest-sha256-v1"` and
  `chunk_format: "cxt-doc-chunks-v2"`;
- the exact canonical CIR envelope;
- ordered chunk occurrences, each with `hash` and `bytes`;
- `stream_bytes` and `event_count`.

Chunks contain the comma-separated canonical event-array interior at fixed
512 KiB boundaries. Only the final chunk may be shorter. Empty conversations
have zero chunks and zero events. Repeated chunks remain repeated occurrences
in the manifest. The same body bytes can be shared without changing event
order, the envelope or the declared counts.

Bounds are 256 KiB per manifest, 2,048 chunk occurrences and 512 MiB for the
reconstructed document including its envelope and JSON framing. The manifest
has one canonical encoding: duplicate, unknown, case-aliased, null or missing
fields, invalid UTF-8, noncanonical escapes, overflow and trailing data are
rejected. Existing CIR version and event semantics remain in force.

## Verification and cancellation

The explicit manifest verifier loads and hashes every current chunk occurrence
and validates complete events across chunk boundaries, event count and
sequence order. A metadata-only hash operation does none of these checks.
Loaders must honor cancellation and must not concurrently mutate a returned
body. A reused loader buffer is supported; the verifier retains its own bounded
chunk copy. One complete event can be larger than a chunk, so event validation
memory is not bounded to 512 KiB.

The materialized-CIR builder is for already bounded, in-memory inputs. It
canonicalizes the full document before checking its output size. Untrusted
external loading must use bounded readers and the context-aware manifest
verifier. Neither helper grants storage ownership or permission to publish.

This preparation adds no durable verification certificates. Existing body
checks remain mandatory. A metadata catalog root, completed upload, cache hit
or known chunk ID never substitutes for current-byte verification.

## Rollout requirements

Before enabling a root writer, all of these paths must support the tagged
identity and reject incompatible peers before side effects:

1. Local and server storage, body reads, event pages and search indexing.
2. Snapshot/catalog metadata, import, initialization, retention and accounting.
3. Chunk uploads, durable finalization jobs, receipt validation and restart.
4. CLI capture, push, pull, verification and interrupted synchronization.
5. Repository-level compatibility checks, including metadata-only operations
   and a concurrent first-root publication.

Root support will be negotiated independently from CIR version, chunk format
and metadata catalog capabilities. An old peer's empty missing-object list is
not proof of compatibility. A previously created root cannot be relabeled or
rehashed into a legacy object during retry. Creating a legacy equivalent would
be a separate object with separate provenance, never an implicit fallback.

Until those gates are complete, no production-facing capability advertises
root support and no capture path creates roots. Domain and metadata tests are
preparation, not end-to-end root support.

Root hashing is proportional to manifest size. Full current-byte verification
still reads existing bytes, and the flat manifest remains proportional to chunk
occurrences. Incremental body writes alone do not establish incremental
verification, constant memory or constant-time search-index publication.
