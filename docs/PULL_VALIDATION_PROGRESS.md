# Resumable local document verification

Issue: [#272](https://github.com/wnsdy95/cxthub/issues/272).

## Problem and boundary

A metadata-only pull can reference large documents already present in the CLI
replica. Previously the application reconstructed each document, decoded CIR,
and canonicalized it again before accepting snapshot metadata. Cancellation
discarded that validation progress. The next hook repeated the same prefix even
though no document needed downloading.

The CLI now uses an optional `StoredDocumentVerifier` outbound port for local
document validation. Both streaming pull's staged-body check and batch validation
use this port. Stores without it retain the existing `GetDoc` plus canonical
validation path. The server API, document identity, chunk formats and timeouts
are unchanged. Cloud backend data remains authoritative; these receipts only
accelerate validation of the local replica.

## Verification contract

On a cold read, FileStore captures a digest of every stored file's exact bytes
as those bytes are consumed for reconstruction. It then runs the existing typed
CIR canonical hash validator. Only successful, uncanceled validation can produce
a receipt. A receipt contains hashes and format/version metadata, never the
transcript, memory contents or credentials.

Receipts live in `.cxt/doc-verification/<document-hash>.json`. Their HMAC-SHA256
key lives outside the repository, in the private user cache directory:

- macOS: `~/Library/Caches/cxthub/doc-verification/key-v1`
- Linux: `$XDG_CACHE_HOME/cxthub/doc-verification/key-v1`, or the OS default
  `~/.cache/cxthub/doc-verification/key-v1`

The composition root explicitly enables the cache; constructing a FileStore
does not create a key or touch the user's home directory. Key creation uses a
private directory, a 0600 temporary file and an exclusive link, so competing
processes cannot publish a partial key or overwrite the winner. A path resolving
inside the repository is rejected.

On every warm read, FileStore:

1. Authenticates the receipt, expected document identity and verifier version.
2. Opens and hashes the **current bytes** of the document representation and
   every chunk in the receipt, with bounded read buffers and cancellation checks.
3. Reuses the canonical proof only when all those hashes match.

It does not trust file existence, size, mtime, or a `verified` flag. Changed,
missing or repacked files invalidate reuse. Shared chunks are read and hashed
again for each document: a previous document's read cannot hide a subsequent
mutation. There is no retained plaintext or unbounded in-memory body cache.

An absent, unreadable, oversized, malformed or unauthenticated receipt falls
back to full validation. Unavailable cache storage or a lost/invalid key also
falls back safely. Cache writes are best effort and never decide whether valid
data may be adopted. Receipts are capped at 4 MiB; documents exceeding that
receipt size remain supported through full validation. Removing the cache does
not remove any context data. A verifier change affecting accepted canonical/CIR
semantics must increment `docVerificationVersion`.

## Integrity and cancellation

The trust boundary includes deliberate modification of `.cxt`: editing objects
and their receipts together cannot manufacture a valid HMAC. This is not a
defense against compromise of the OS account, CLI binary, or private key itself.
As with normal object reads, it verifies bytes observed during the operation;
it cannot prevent an external process from corrupting files after verification.
`fsck` and ordinary document reads retain their independent integrity checks.

Completed document receipts survive cancellation and can be used by a new CLI
process. A partially verified document gets no receipt. Snapshot dependencies,
repository identity, graph cycles, refs, causal memory dependencies and metadata
conflicts still pass through the existing batch checks. Ref/working-position
adoption and remote state cursors remain after those checks. A receipt never
authorizes partial metadata publication.

The cold path still performs full reconstruction and canonical validation of
each previously unverified document. This change reduces repeated work; it does
not make a first-time validation of arbitrary-size input constant time. JSON
decode/canonicalization itself remains synchronous, with cancellation checked
around it. No hook or network timeout was increased.

## Validation and measurements

Regressions cover legacy blobs and v1/v2 manifests, a new FileStore/process using
completed receipts, repacking, same-size/same-mtime corruption, missing chunks,
symlinks, receipt forgery, key loss, unavailable cache writes, unsupported CIR,
noncanonical event order, concurrent key/receipt publication and cancellation.
An application-level metadata-only pull test cancels after the first validated
document, verifies no snapshots/refs/cursors were adopted, then completes with
a fresh store while preserving the first receipt.

Local measurement on Apple M5 Pro, using a pre-existing 166,003,658-byte document
with 44,991 events and 317 chunks (no private data committed as fixtures):

| Operation, separate CLI process | Elapsed validation | Cumulative allocation | Peak RSS |
| --- | ---: | ---: | ---: |
| Previous GetDoc + canonical validation | 3.326 s | 3.597 GB | 1.578 GB |
| First validation and receipt | 3.186 s | 3.799 GB | 1.512 GB |
| Receipt reuse, all stored bytes checked | 0.029 s | 1.192 MB | 6.898 MB |

The warm process read and hashed 65,864,123 stored bytes across 318 files. The
comparison isolates local reconstruction/verification; it involves **no download**
and is not a claim about network transfer speed. The OS filesystem cache may be
warm. Peak RSS was measured with `/usr/bin/time -l`; cumulative allocations with
Go runtime counters. Cold cost is intentionally retained to prove semantics.

A reproducible 10 MiB synthetic benchmark is included:

```sh
cd cli
go test ./internal/adapters/storage -run '^$' \
  -bench BenchmarkStoredDocumentVerification -benchtime=3x -count=1
```

Observed cold: 136.9 ms / 164.7 MB allocated; warm with a fresh store: 3.15 ms /
95.2 KB allocated. Timing is diagnostic, not a brittle CI assertion.

## Alternatives considered

- An in-memory-only proof cache cannot preserve work across hook cancellation
  or CLI restarts.
- File-stat receipts are cheaper to implement but miss same-size/same-mtime
  corruption and are forgeable with the replica.
- Authenticated receipts plus current-byte hashing preserve the established
  domain validator while removing repeated inflate/decode/canonicalize work.
  They add disposable per-user cache state and retain disk IO/hash costs.

Streaming the domain canonicalizer could reduce cold peak memory further, but
changing typed union handling, event ordering and number normalization belongs
in a separately verified compatibility change.

## Unchanged ref projections

End-to-end dogfood also exposed repeated ref replacement after validation: one
timed pull finished dependency preflight at 9.73 s, lifecycle adoption at 22.32 s,
and ref adoption at 64.09 s. The server returned 2,058 refs, mostly retained-history
tags. Rewriting already-equal values repeatedly ran durable file/directory syncs.

FileStore now compares the installed branch/session/tag representation **inside
the existing ref lock and after the existing lifecycle checks**. Equal values
are idempotent no-ops; changed values retain the same atomic durable write path.
The optimization does not skip branch-policy checks, release the lock earlier,
change HEAD/worktree handling, or move refs before pull preflight. A regression
asserts that an equal physical ref cannot resurrect an archived logical branch.

Pull also uses the optional `ExistingTagVerifier` port to recognize equal tags
in one ref transaction, avoiding one durable legacy-lock acquisition per already
installed history tag. Only complete current-value matches are acknowledged as
no-ops at that serialized observation point. Missing or changed tags still follow
the existing conflict checks and writes. Branches cannot use this path; lifecycle
events are applied through their existing policy before ordinary ref adoption.
There is no batch of deferred mutations or new partial-ref commit protocol.
