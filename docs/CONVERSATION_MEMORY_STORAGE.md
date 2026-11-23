# Conversation storage

The conversation of a session is stored on the server as one document. This
page describes how that document is stored and identified. What a session is,
and who may read which part of it, is described in the
[architecture overview](ARCHITECTURE.md) and the [CLI reference](CLI.md).

## Conversation

- A document is stored as immutable content-addressed chunks and their ordered
  manifest.
- An upload transfers only chunks the destination repository does not own. A
  session that grew sends what was appended.
- Byte-chunk identity is distinct from event identity: one event of a
  conversation can cross a chunk boundary.
- A session's report names its document and, for every segment and for the
  uncommitted part, where the range starts in the document and how many events
  it holds. The report is stored separately from the document.
- When a report names another document than the one before, because the
  session grew or was masked another way, the earlier document is deleted when
  no other report uses it. The pieces it was stored in stay in the server's
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
5. Store the session's report, which names the document, only after the
   document is on the server. A report that names a document the repository
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

## Required validation

1. Appending to a conversation changes only the new and tail chunks; chunks
   already stored keep their identities.
2. Changed, missing, unowned, reordered or malformed chunks cannot produce a
   valid publication. Cancellation and expired job leases publish nothing.
3. Measure newly transferred bytes, bytes read and hashed, parsing,
   allocations, indexing and completion wait separately. Reduced allocation is
   not proof of reduced cold indexing time or end-to-end transfer latency.
