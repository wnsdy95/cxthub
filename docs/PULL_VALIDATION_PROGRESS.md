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


## Reusing the verifier during history selection

Tracking: #290. History source validation previously decoded the same document
separately for Source, Target, SharedTarget, MemorySource and inherited memory.
A synthetic event naming the same document in all four roles performed five
full decodes. This bypassed the authenticated receipt already available to pull.

History selection now uses the same `StoredDocumentVerifier` capability. A
per-operation set removes repeated verification of one immutable document; a new
operation always checks current stored bytes again. Snapshot repository ownership
is checked before document verification, memory provenance remains exact, and
narrow stores retain a full decode/hash/semantic-validation fallback. The fallback
also verifies that the returned document hash is the requested hash.

A repeated-text 16 MiB synthetic transcript on a local Apple M5 Pro measured:

| History validation | Before | After |
| --- | --- | --- |
| Cold, no receipt reuse | 1.24-1.31 s | 0.26-0.28 s |
| Warm authenticated receipt | 1.21-1.22 s | 1.0-2.9 ms |
| Allocated bytes, cold | 1.13-1.24 GB | 0.22-0.27 GB |
| Allocated bytes, warm | 1.23-1.28 GB | 0.17 MB |

Allocation totals are not peak RSS. The highly compressible fixture measures the
history verification path, not whole-push latency or cloud throughput. Cold work
still fully validates the canonical document. Reproduce with:

```sh
cd cli
go test ./internal/app -run '^$' -bench '^BenchmarkHistorySourceVerification$' -benchtime=1x -count=3
```

Failure tests cover altered bytes with unchanged size/time, deleted documents,
foreign repository metadata, wrong fallback hashes, cancellation and mandatory
re-verification on the next operation. The existing storage receipt suite covers
forgery, repacking, private-key placement and concurrent key creation.

## Incremental server search projections

The PostgreSQL search table already deduplicated canonical event hashes, but a
new document still decoded, extracted, lowercased and transmitted every inherited
event's text before `ON CONFLICT DO NOTHING`. A verified document now produces a
read plan first. The adapter asks for existing v2 search hashes, retains those rows
with transaction-scoped key-share locks, and extracts/transmits text only for new
events. Offsets, sequence and role still come from the new document. A search row
never confers document ownership or skips whole-document verification.

The namespace/version remains unchanged because projection semantics are exactly
the same. Concurrent last-owner deletion cannot invalidate a reused row before
publication; document ownership, event locations and new search rows still commit
or roll back together. Tests cover parallel append documents, retained cache rows,
parent deletion, last-owner cleanup, literal Unicode/wildcard search, foreign
repository rejection, canonical order, escapes and unverified plan rejection.

For a synthetic 16 MiB inherited prefix on an Apple M5 Pro, constructing the read
projection measured 177–183 ms / 67.4 MB allocated for cold text extraction versus
84–85 ms / 33.7 MB with existing search rows (3 iterations, two samples). These
numbers exclude initial schema/hash validation, SQL, compression and network;
they are not end-to-end push or production throughput claims. Reproduce with:

```sh
cd backend
go test ./internal/domain -run '^$' -bench BenchmarkReadPlanInheritedPrefix -benchtime=3x -count=2
```

FS search filters retain their persisted FNV-1a byte-trigram layout but visit
slots directly rather than allocating one uint32 per source byte. A synthetic
15 MiB string measured approximately 30 ms with zero loop allocations. Legacy
bit-parity tests prevent false negatives against existing filter files. FS is
still the development compatibility adapter pending the PostgreSQL cutover.

## Foreground publication and retained history

Ordinary `cxt push` and Git pre-push now publish the prerequisite closure of
refs, durable history, pending pointers and queued graft/promotion operations.
Selection includes natural and overlay parents, memory fragments, pinned sources
and earlier memory generations. An authenticated server memory attachment can
satisfy an already-published immutable memory dependency without decoding it
again locally. A missing or changed attachment still follows full validation.

Unrelated retained snapshots are not a prerequisite for moving current refs.
Their missing objects and memory attachments are first recorded in
`.cxt/historical-backfill/<repository>/<snapshot>.json`, before foreground
history or ref publication. A failed queue write stops foreground publication.
The records pin local documents against sliding-capture collection. They contain
hashes, attempt counts, retry times and safe reason codes, not transcript text or
credentials. Changed requested state increments a version, so a delayed worker
cannot acknowledge newer work. No source document is removed by this queue.

A separate local helper uploads up to eight jobs per batch with exponential
backoff capped at 256 seconds. It holds an OS worker lock during each batch,
releases object-retention leases between jobs, and never changes refs, history
events or pending pointers. Existing causal memory CAS and conflict checks are
shared with foreground sync. The helper has a 30-minute lifetime; the queue
survives exits, cancellation and machine restarts, and the next push wakes it.
This is durable local retry, not an always-on OS scheduler. Git hook timeouts
are unchanged. Narrow adapters without coordinated retention retain full push.

`cxt sync status` / `cxt sync status --json` read only the local queue and branch
journal metadata. They show eligible/waiting jobs, attempts, safe failure categories
and retry times without loading document or memory bodies, contacting the server,
starting a worker, or changing files. Eligibility does not claim that a worker is
currently running. Empty queues do not prove server acknowledgement or integrity;
JSON explicitly reports those checks as false. Missing/corrupt metadata is an
error, not an empty successful queue. `cxt doctor` remains the separate full local
object/reference audit and also includes these queue details.
`cxt push --wait-history` explicitly waits for the original full publication
path and reports failures synchronously. Neither command turns a failed upload
into a successful ref publication. Fresh dependencies that arrive during a push
remain the next sync's obligation; an existing publication never consumes a
newer pending pointer accidentally.

Failure tests cover unavailable queue storage, required document failure,
unrelated archival failure, process/store restart, cancellation, worker exclusion,
stale acknowledgement, corrupt/symlinked queue records and capture collection
before/after acknowledgment. Memory-only provenance and remote proof reuse have
separate selection regressions. Synthetic tests prove ordering and durability;
they do not claim that every live historical upload has completed.


## Explicit synchronization progress

User-invoked push/pull reports application phases to stderr: preparing the
catalog, negotiating missing objects, validating dependencies, transferring and
verifying documents, recording snapshots/memory/history, and publishing or
adopting refs. Document and snapshot counters advance only after successful
acknowledgement. Counters are phase-local, not a fabricated overall percentage.
A concurrent collection retry starts a new prerequisite pass; failed or cancelled
publication never reports synchronization complete. Hook/background calls omit
the optional observer. Foreground completion does not claim historical backfill
is finished: the resulting retained queue count remains separate.
# Uploaded-chunk persistence cost (2026-09-29)

PostgreSQL finalization previously compressed the entire canonical document even
when it immediately replaced that result with a small chunk manifest. It also
compressed and sent every already-uploaded chunk to an INSERT that would lose
its conflict, before reading and verifying the existing bytes.

Finalization now compresses only the selected storage representation. Existing
chunks are read under a key-share lock, decompressed and compared with the
verified canonical plan before ownership is granted. Missing chunks are inserted;
if another writer wins that insertion, its bytes are read and checked too.
Manifest, chunk ownership and index publication remain in the same transaction.
No stored hash, wire contract or migration changes. Presence is not integrity
proof, and sharing a chunk does not grant access to another repository's document.

Run against a disposable PostgreSQL 16 database, never a development/user DB:

```sh
go -C backend test -tags postgres ./internal/adapters/store -run '^$' \
  -bench BenchmarkPGUploadedChunkFinalization -benchtime=3x -count=3 -benchmem
```

`CXT_TEST_DSN` supplies the disposable database connection. The synthetic 8.2 MB
fixture has compression-resistant event bodies. Uploaded chunks and matching
event indexes are already present; each iteration changes only its envelope.
Canonical verification is outside the timer. On the same local machine, the
median of three runs changed from 160.6 ms / 99.8 MB allocated to
110.2 ms / 51.8 MB allocated (about 31% less time and 48% fewer allocated bytes).
The ranges were 150.4-165.7 ms before and 108.0-129.1 ms after. These are total
allocations, not retained heap measurements.

The full backend suite and vet passed with and without PostgreSQL. Focused race
tests verify corrupt reused bytes, transactional rollback, foreign-document
isolation, GC blocking and concurrent insertion with matching/corrupt winners.
This measures persistence only. Network transfer, canonical verification,
worker scheduling and queue wait remain separate costs; it is not an end-to-end
latency or cloud throughput guarantee. Live synchronization may still time out
and retain durable work for retry.

## Reusing canonical event verification

Document finalization now checks canonical wire bytes one event at a time and
keeps at most 65,536 process-local event proofs. Each key includes the hash of
the exact current bytes and the CIR version; its value is the validated sequence
number. No transcript, untrusted `verified` flag or repository authority is cached.
Eviction and process restarts fall back to ordinary typed event validation.

Every call still checks the complete document hash, exact envelope/framing and
event order. A cold event passes the same typed union, optional-field, replacement
history and canonical JSON rules as `VerifySessionDoc`. A warm event cannot move
v2-only fields into a v1 envelope. The returned document owns an immutable copy.
Chunk ownership and current chunk bytes are verified before this optimization;
transactional publication and lease fencing remain unchanged. Legacy stored
noncanonical representations retain their existing decoder path.

The borrowed event scanner runs only after JSON framing validation. Differential
fuzzing compares acceptance with the prior full-document decoder/canonicalizer.
Regressions cover changed hashes/bytes, unknown fields, duplicate JSON keys,
sequence and replacement order, CIR downgrades, numeric normalization, eviction,
concurrent callers, canceled work and foreign or corrupted chunks after warming.

Run the synthetic benchmark without private transcripts:

```sh
go -C backend test ./internal/domain -run '^$' \
  -bench BenchmarkCanonicalDocVerification -benchtime=3x -count=1
```

An 18.9 MB cumulative fixture on the local Apple M5 Pro measured 256.8 ms /
250.8 MB allocated for the previous typed pipeline, 330.8 ms / 209.2 MB for a
cold verifier, and 72.0 ms / 19.0 MB after a verified prefix plus a new event.
The warm path saves about 72% time and 92% allocated bytes; the cold path costs
about 29% more time. This deliberately preserves first-time semantic validation.
Workloads exceeding the cache capacity may not benefit. These numbers isolate
verification; they exclude transfer, persistence and queue wait.

## Repair uses one verified ref observation

The repair command previously fetched and verified objects, then fetched a newer
remote manifest for its ref list. A concurrent push between those reads could
select an object absent from the staging directory. `SyncOutput.FetchedRefs`
now carries the refs verified by that exact pull, separately from adopted local
refs. Repair consumes this observation without a second manifest read. A newer
server tip is left for the next synchronization, and healthy local-ahead refs
remain intact. A missing prerequisite names the ref and target before live writes.
A deterministic regression advances the server manifest after pull and proves
that the verified observation repairs successfully while the newer one is refused.
Tracking: #305.
