# Conversation identity contract

A conversation document has one of two identities, and every read, upload and
negotiation of a document names which one it uses. New root publication is
opt-in: it requires a compatible released binary, the server admission setting
and the repository requirement. Existing documents retain their canonical-CIR
IDs. An incompatible binary or peer must reject a root reference; it must never
treat it as a legacy hash.

## Identity and representation

The absent or empty `identity` on a document means the SHA-256 of its
canonical CIR bytes. The field is omitted for such documents, preserving their
wire bytes. Identity metadata is immutable: a stored document cannot be
relabeled.

The `cxt-manifest-sha256-v1` scheme hashes the exact bytes:

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

Body checks remain mandatory when a document is finalized and whenever its
bytes are read. A metadata catalog root, completed upload, cache hit or known
chunk ID never substitutes for current-byte verification.

There is one durable verification record: the completed finalization job. A
job becomes completed only in the transaction that verified the whole document
and published it, and its identity binds the repository, the document hash and
the exact manifest: the chunk manifest of a legacy v2 document, or the
canonical root manifest of a root. The record shows that the document was
verified once. It does not show that the stored bytes are still intact: damage
after finalization is found when the bytes are read.

Finalizing a legacy v2 document still reads and hashes every chunk once. The
legacy hash covers the envelope before the events, and the envelope changes
between uploads, so that pass cannot resume from an earlier upload. What a
finalization can skip is the repeated prefix. When the repository owns an
earlier legacy document that this process published, and that document's stored
manifest starts with the same chunks, finalization handles those leading chunks
differently:

- it does not write them to its spool, and reads of that range while indexing
  load the stored chunk again and check its hash;
- the publishing transaction locks those chunk rows and the earlier document's
  ownership row instead of reading and comparing the chunk bytes a second time.

If the earlier document is no longer owned at publication, every chunk is
checked as before. The hint that names the earlier document is per process, so
after a restart the next document is finalized in full. This targets the last
document of a session that goes on: until it is full, each upload of it
repeats almost all of the previous one.

## Rollout requirements

Before enabling a root writer, all of these paths must support the tagged
identity and reject incompatible peers before side effects:

1. Local and server storage, body reads and read indexing.
2. Chunk uploads, durable finalization jobs, receipt validation and restart.
3. The CLI's upload of conversation documents, including an interrupted one.
4. Repository-level compatibility checks, including a concurrent first-root
   publication.

Root support is negotiated independently from CIR version and chunk format. An
old peer's empty missing-object list is not proof of compatibility. A
previously created root cannot be relabeled or rehashed into a legacy object
during retry. Creating a legacy equivalent would be a separate object, never an
implicit fallback.

New publication requires production repository transactions. Admission is off
by default. Disabling it after opt-in keeps existing root reads available; it
does not remove the repository requirement or make old clients compatible.

Before repository opt-in, replace and verify every API, MCP, worker and
maintenance binary that can access it. A schema migration alone cannot protect
data from an old binary. See [conversation storage](CONVERSATION_MEMORY_STORAGE.md)
for admission settings and [CLI configuration](CLI.md) for the `capture.identity`
preference. Configuring it checks the server; every upload checks current
admission again.

Root hashing is proportional to manifest size. Full current-byte verification
still reads existing bytes, and the flat manifest remains proportional to chunk
occurrences. Incremental body writes alone do not establish incremental
verification, constant memory or constant-time read-index publication.
