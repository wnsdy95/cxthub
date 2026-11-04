# Repository-view source journal

Status: local D3 foundation, opt-in durable On Hold reads and a worker-only graph publisher; no production cutover. Follow
[the design](INCREMENTAL_READ_PUBLICATION_DESIGN.md) and the
[dependency audit](PROJECTION_MUTATION_AUDIT.md). This scope deliberately does
not certify Git/PR evidence, effective memory, document bytes or authorization.

## One clock, compatible consumers

Migration 0099 reuses `repository_catalog_state.epoch/head_seq/writer_xid`.
Public snapshot/ref/history/protocol images stay in the existing catalog;
only the previously missing input kinds use a companion table. The private
`repository-view-v1` port reads their union through one committed checkpoint.
There is no second ordering clock or duplicate snapshot/history ledger.

The new kinds are repository metadata, pending pointers, unsync pointers, raw
session-archive records, reflog entries and revision counters. Row triggers cover
ordinary adapter calls and SQL writes. Known fields are explicitly projected;
arbitrary JSON fields, conversation bodies and secrets are not copied.
Stored origins are sanitized in the new image instead of copying legacy URL
credentials. The original repository rows are not rewritten.
The conservative SQL sanitizer accepts ordinary ASCII DNS/IPv4 origins and
standard URI/SCP paths. IPv6, Unicode or otherwise unsupported nonempty origins
produce an empty image plus `remote_url_supported=false` or
`git_remote_url_supported=false`, not an assertion that the repository has no
origin. Origin-dependent consumers must reject that unsupported projection;
full equivalence with the existing Go URL policy remains a cutover requirement.

The catalog state row serializes sequence allocation. All covered changes in
one top-level transaction share the sequence; repeated changes to one key keep
the final image or tombstone. Savepoint/outer rollback also rolls back these
images and allocation. The sequence is database commit order, not Git ordering.
Deadlocks/failed commits abort; this is not an exactly-once delivery promise.

Existing CLI catalog APIs still emit only their original four kinds. A private
update can advance their checkpoint without returning public entries. Sequence
gaps are valid: callers must not infer a missing public mutation from them.
Old binaries can continue reading the unchanged public table; application
rollback must retain the additive migration and its triggers.

## Bootstrap and maintenance

The migration locks all covered source tables, installs capture and seeds the
missing images at the current head `H`. A private baseline marker records `H`.
Public epoch, retained history and CLI cursors remain unchanged. The new reader
rejects a checkpoint before `H`; the seed does not invent past private states.
New repositories initialize both scopes in the same transaction.

The existing reset, seed and prune entrypoints cover both journals. Reset
rotates the shared incarnation and seeds at zero under source locks. Prune keeps
the latest image/tombstone at or before the floor for each key, then advances
the shared floor. Covered source TRUNCATE is rejected. Offline restore still
requires an explicit epoch rotation; a restored database cannot detect rewind.

Consumer leases/watermarks that constrain retention are **not implemented by
this foundation**. Any reader whose floor/epoch changes must discard incomplete
work and bootstrap again. Durable workers must add retention protection before
the production read-model cutover; do not hide rebuild starvation with retries.

## Fixed, bounded reads

`RepositoryViewSourceChanges` is an outbound store port, not an unauthenticated
HTTP endpoint. It requires a committed owned read snapshot, checks current
document-identity policy and rejects a caller-owned write transaction. Future
application consumers must also enforce current repository authorization.

- Baseline pages select latest retained images at fixed `Through`; delta pages
  select changes after a complete checkpoint through that same fixed head.
- Pagination reads journal images, never a mixture of paged current tables.
- Cursors bind scope, version, repository, incarnation, coverage start, floor,
  mode and keyset position. They select data; they do not grant access.
- Only the last page publishes a checkpoint. Consumers stage partial pages;
  they must not apply half a transaction as a complete generation.
- New sequence values use JSON decimal strings. Reflog keys preserve the
  original numeric row ID; lexical pagination is not chronological ordering.
- Defaults are 128 entries, maximum 1,000. Descriptor reads transfer lengths
  and bounded keys, not all candidate images; a second query fetches only the
  selected prefix in the same read snapshot. Draining descriptors cannot drain
  unseen image payloads. An image is limited to 1 MiB, a key to 4 KiB and the
  estimated page cost (images, keys and fixed per-row overhead) to 4 MiB.
  Oversize metadata/cursors fail explicitly, without omission or truncation.
  Descriptor sizing still costs database work over at most `limit + 1`
  candidates; this is not a claim of constant-time whole-history bootstrapping.

## Still required

Keep existing full-input graph fingerprints and live authorization checks.
Complete dependency coverage and durable graph/inclusion/effective-memory
models; On Hold does not certify those consumers. Compare their reconstructed
semantics against authoritative facts before switching Web or MCP. This work
does not close D3, D4 or the release gates.

## Opt-in durable On Hold consumer

Migration 0100 adds repository-scoped work, staged source images, versioned
result parts and an atomic ready head. No conversation or memory body is copied.
The consumer calls the existing domain publication/hold/archive rules rather
than inventing a second classification policy. Ref display order is bytewise
canonical in the shared domain projection, independent of DB collation.

The workflow is `capture → project → idle`, with persisted retries and terminal
`needs_attention`. Each captured page and its continuation commit together;
only the last source page allows projection. Completed parts and their ready
head publish together. Another worker cannot publish with an expired or
superseded lease. Database time controls leases; claims and processing have
bounded deadlines and share the existing bulk-admission budget.

Enabled consumers pin the shared journal retention floor. A pin expires after
24 hours without worker progress; it does not retain history indefinitely.
An epoch/floor incompatibility requires a new dataset and baseline, not reuse
of a partial old capture. Original journal/history retention remains distinct
from retention of reconstructible projection state.

Superseded derived input rows are collected with capture progress; output rows
are collected with ready-head publication. The current ready dataset remains
available while a replacement builds. Existing repeatable-read transactions
retain their own MVCC view. Deleted-input markers remain until capture is
complete, so losing a partial cursor cannot silently replay a half-capture;
inconsistent progress forces a fresh dataset. There is no API for historical
On Hold generations. Original snapshots, conversations, memory and source
journal entries are not collected by this code. Normal PostgreSQL vacuum and
physical storage reclamation remain operational concerns.

For a ready read, the header lists the current entity keys. Indexed latest-row
lookups fetch only those keys, with row/byte checks before body transfer. Reads
never list all original snapshots or load PR evidence. Build input is limited
to 100,000 active images/32 MiB; output is limited to 20,000 parts/4 MiB,
including key and per-row overhead. These are explicit opt-in limits, not
permission to truncate large repositories. Projection computation still
classifies the bounded current metadata set; this is not yet a fully
incremental graph algorithm.

### Rollout and wire contract

`CXT_PENDING_PROJECTION_REPOS` accepts a comma-separated list of exact repository
content hashes. Empty/unset preserves the existing direct query path and does
not start this worker; wildcards and malformed IDs fail startup. Configure it
only after migration and local shadow/parity validation. Each process serves
durable reads only for its current allowlist. Durable work enablement persists
in PostgreSQL so another configured worker can continue it; removing a reader
allowlist entry is not a global disable command and must not disable another
replica's work. Drain/disable workers deliberately during rollback.

`projection=on-hold-v1` remains explicitly separate from the complete graph.
An enabled, fully caught-up result adds `read_generation`, including the
source/applied `UUID:decimal-sequence`, algorithm version and complete flag.
Missing or lagging results return `503 pending_projection_pending` with
`Retry-After: 1`; the request does not synchronously rebuild or relabel an old
generation as current. Terminal failure returns `pending_projection_failed`
without automatic retry advice or raw stored error details. Current repository
access is revalidated after admission; document identity remains checked inside
the coherent read. Neither a generation hash nor its source cursor grants access.

No API/MCP split, original object identity, PR completion record or context
ancestry changes as a consequence of enabling this read model. MCP/full-graph
cutover is separate work. Returning to the direct path preserves authoritative
records and the additive migrations.

The revision endpoint and SSE now optionally carry `pending_projection` alongside
the unchanged graph/pending/evidence counters. It names the source incarnation
and sequence, rules version, and `disabled`, `catching_up`, `ready` or
`needs_attention` state. Only `ready` includes the same generation hash as the
On Hold body. A worker finishing or a source-only mutation can therefore trigger
a notice without changing the legacy counters. Missing status preserves the
non-opt-in response. SSE event IDs are opaque hashes, not ordered counters.

Status reads use only repository identity, catalog/source head and work/ready
metadata in one read snapshot. They never read parts, inputs, source images or
conversation bytes. One watcher per repository/identity class shares this
small read among browsers; stream lifetime and per-request access checks remain
unchanged. Status reports publication readiness, not a fresh full-payload
integrity audit; the body reader still validates the selected result parts.
Repeated identical notices must not trigger recurring body downloads. Do not
interpret the opt-in or local parity tests as permission for a production
version-only cutover.

The browser tracks these notices separately from body caches. Known catch-up
and attention states gate payload requests; ready results, including manual
Retry, must match the advertised generation. Overlapping notices receive bounded
follow-up reads instead of being forgotten or retried in a tight loop. Status
authorization denial hides cached rows/viewer and fences late completions;
stale SSE cannot clear it. Existing graph queries retain fresh mount reads while
their independent durable-generation contract remains unfinished.

## Validation checkpoint (2026-10-19)

The source, public catalog/Merkle and metadata-generation suites passed three
race-detector repetitions against disposable PostgreSQL. A separate three-run
budget suite observes returned driver values, explicitly drains descriptor rows
and proves that only selected image payloads transfer. Failed-migration rollback
and successful retry preserve existing CLI checkpoints. The unchanged CLI
validator/cache/client suites also passed with new sparse-generation tests.

Only fixture databases were migrated. No production query uses this port yet. Keep
0099's validated checksum unchanged; any subsequent schema correction requires
a new migration or a completely fresh, never-published fixture iteration.

The subsequent D3b consumer and notice extension passed focused PostgreSQL race
tests, the complete normal backend suite, all frontend contract suites and 61
combined browser regressions. Metadata-only status tests require three point
queries with at most three rows and 512 observed driver-value bytes, unchanged
after adding 4,096 historical keys to each payload relation. The full body reader
still rejects corrupt/missing/oversized selected parts. See the implementation
plan's D3b checkpoints for individual runs and remaining gates.

## Graph source boundary (D3c, 2026-10-19)

Migration 0101 adds `repository-graph-v1`, a separate private scope sharing the
catalog incarnation and transaction sequence. It includes the complete
`repository-view-v1` stream plus fixed after-images/tombstones for:

- Git commit parents, directory objects, deltas and ref observations;
- PR history checkpoints, participation proofs and contribution receipts;
- ordinary Git contribution receipts, head receipts, admissions, context heads
  and contribution proofs.

The source image includes the actual lookup columns and checkpoint version as
well as its typed payload. An ID or current mutable row cannot substitute for
the captured image. The two directory/delta kinds prepare memory assessment;
their capture alone does not complete effective-memory dependencies. Contribution
memory pins, legacy memory metadata, immutable bodies and ownership still need
their own fixed-source/access contracts.

The new scope does not add kinds to either older scope. Installation records a
new coverage baseline without rotating existing epochs or rewriting old catalog
history. Its reader requires both private baselines and the shared retention
floor. Cursors cannot cross scopes or repositories. Reset reseeds all three
journals; prune retains each key's floor anchor and keeps the existing pending
consumer pin. A future graph consumer must add its own retention pin before
relying on replay after a long interruption.

Graph, branch-inclusion and segment-basis process caches now require an explicit
graph-source-generation port. Its metadata-only read is in the same read
snapshot as the actual inputs. A plain metadata generation or G/P/E tuple does
not certify evidence coverage. Unsupported adapters recompute; epoch reset and
source-only evidence changes cannot reuse a former entry. The graph's coherent
input fingerprint is retained. None of these caches stores authorization or
memory bodies, and current document-identity checks remain on the read path.

An empty, complete On Hold source delta can advance its existing ready watermark
without loading inputs, recomputing the domain projection or rewriting parts.
This requires no previous partial cursor, an existing matching ready dataset,
matching rules, and a live worker fence. Metadata and worker progress advance in
one transaction. Baseline capture and a partial-delta tail still use normal
projection. Thus adding private graph evidence does not by itself force a full
On Hold rebuild, although the common watermark and notice generation advance.
An evidence transaction that also emits a revision image is not empty and still
uses the normal path; dependency-specific projection updates remain future work.

This is **source capture and process-cache correctness**, not a durable graph
cutover. Before a captured graph serves users, its adapter must reproduce current
proof joins and typed integrity checks, reject unsupported origins, constrain
nested payload fields without changing identity-hashed evidence, and compare
old/new graph and inclusion results. The private journal does not contain auth
tables or conversation/source-file bytes, but its raw proof payload is not a
public DTO. Full memory coverage, scope-specific invalidation, durable graph
rows, worker recovery, rollback and production performance remain release gates.

### Frozen graph reconstruction (D3d)

The offline/shadow capture primitive now requires an entire baseline or delta
ending at one `repository-graph-v1` checkpoint. It validates fixed repository,
epoch, upper sequence and scope across all pages, strict page order, bounded
cursor/image/row sizes and mutually exclusive continuation/final markers.
Tombstones are applied only in a delta built on a complete prior image. A failed
page, reset or prune returns an error without replacing that prior image.

`CapturedGraphSource.Inputs` separates repository-view metadata from the twelve
proof kinds. Graph metadata decoding requires both origin-support flags;
missing flags or unsupported nonempty origins cannot be interpreted as absent
Git evidence.
The On Hold decoder remains origin-independent. The frozen proof adapter checks
typed envelopes, nested payloads, lookup identities and receipt/admission/head/
contribution joins using validation shared with the SQL decoders. It never
normalizes identity-hashed evidence or reads missing rows from live tables.

`ProjectCapturedGraph` has only the read-only evidence contract. It reuses the
same graph/inclusion business rules but has no backing metadata writer, live
cache or provider client. This is not an authorization entry point: no public
route is switched, and future serving must still authorize each request against
current policy before exposing a completed generation.

Real PostgreSQL parity covers PR, ordinary Git, combined deduplication,
historical selection, source-session archive and destination rename. Deleting
live proof rows between capture pages does not alter the pinned source; the
next completed delta reflects the deletion. Reset/prune interrupts incomplete
capture instead of mixing generations. This does not yet provide a durable
graph consumer pin, resumable publication, effective-memory bodies/ownership or
production rollout. Full fingerprint checks on the serving path remain intact.

### Durable graph publisher (D3e)

Migration 0102 gives the graph consumer independent work, inputs, output parts
and a ready head. Its retention pin joins the existing catalog/view/graph prune
checks; it does not replace the On Hold pin. Enabled, unexpired pins for the
current catalog incarnation protect incomplete capture. Disabled, expired and
old-incarnation pins do not retain source history indefinitely.

The PostgreSQL adapter shares fenced claims, page validation, input bounds,
retries and atomic publication with On Hold. Only static internal table specs
select a consumer. The source scope, allowed input kinds, source-baseline SQL,
rules version and output codec remain explicit and separate. A graph consumer
uses the maximum of graph and metadata coverage boundaries. It cannot resume a
metadata-only cursor or substitute live evidence for missing frozen inputs.

Each capture lease saves at most one bounded source page. The final checkpoint
alone makes a dataset eligible for projection. The application reconstructs a
read-only proof adapter from those completed inputs, applies the existing graph
rules, and publishes parts, ready head and applied checkpoint in one transaction.
The shared resource scheduler admits this work before starting its 30-second
work deadline. A stale worker cannot advance the newer owner's state. The prior
complete result survives interrupted reconstruction, source reset and publication
rollback, but is not returned as current when its source or rules differ.

The graph codec persists typed top-level fields rather than a transcript or one
opaque graph blob. A strict header binds the repository, graph/rules version and
the exact field digests. Field normalization tolerates JSONB object-key ordering
but rejects missing fields, duplicate/unknown fields, extra parts and corruption.
Ordering, nil versus empty values, and internal merge identities are preserved.
Only the default server graph is published here; a selected historical position
is reconstructed from the same completed metadata bundle, not current source
rows. The 4 MiB/20,000-item safety limits count
nested memberships too, not 20,000 graph nodes. Oversize graphs fail explicitly,
without truncation; wider scale limits need measurement before serving cutover.

Unchanged completed deltas advance only the ready checkpoint. A rules mismatch
wakes an idle consumer even without a source write and starts a fresh dataset.
Both consumers retain their last complete parts while replacing incompatible
work. Mixed workers with different projection rules are not an approved rolling
upgrade strategy: drain incompatible workers before changing rules.

`CXT_GRAPH_PROJECTION_REPOS` accepts comma-separated exact repository hashes.
It enables the **worker only** in `cxtd`; empty/unset starts no graph worker.
By itself it does not change Web, MCP, graph query routes or fallback policy. Durable
enablement is shared, not a per-process ownership assignment. Removing this
startup allowlist does not revoke another replica's work; operational rollback
must drain or explicitly disable it. Per-repository shadow comparison, serving
cutover gates and effective-memory dependencies remain required. No production
migration or activation is performed by this code.

### Completed graph/view reader (D3f)

`CXT_GRAPH_READ_REPOS` is an independent, default-off exact-repository allowlist
in `cxtd`. The reader requires the complete rules-2 bundle, not graph-only rules-1
parts. Graph and companion metadata are published atomically under the same
ready checkpoint. Capturing the next dataset never overwrites the metadata read
from the previous ready generation. Reader configuration cannot enable a worker.

Full repository, graph and live responses include `read_generation`: generation
ID, applied/source epochs, expected rules, `complete` and `current`. A complete
same-incarnation generation may lag the source. Its payload and applied checkpoint
stay unchanged; only the source observation/current flag changes. Disabled,
failed, old-incarnation or incompatible generations are unavailable. Missing
output returns bounded `graph_projection_pending`; attention-required work returns
`graph_projection_failed`, with no automatic client retry for that failure.
Corruption returns an integrity failure, not an expensive direct-reader fallback.
The opt-in reader also requires a durable graph repair adapter. An identified
completed-output failure carries the exact repository/dataset/applied-checkpoint/
rules receipt; a source, authorization, identity, SQL or cancellation error does
not substitute for that receipt. After its owned read transaction closes, the
application reauthorizes and attempts bounded durable scheduling. Borrowed,
unknown-ownership, write and explicit read-only contexts never escape into a
repair write. Scheduling failure remains an error, not a repair acknowledgement.

Migration 0103 adds one bounded diagnostic/budget record per repository. Repair
rechecks current compatibility and compares the exact ready identity under the
work/source locks. A stale report cannot invalidate a newer publication or a
different source incarnation. Disabled work is not re-enabled; an intervening
terminal worker failure is not cleared. Concurrent reports coalesce, invalidate
only derived readiness, fence the old lease and durably request a fresh capture.
Original snapshots, conversation/memory objects, refs and history stay unchanged.
At most three automatic rebuilds are admitted in a rolling hour. A fourth
distinct failed result requires attention, and restarting/enabling the worker
does not silently clear that quarantine. The existing worker resumes accepted
repairs; a read never rebuilds the graph inline. This local implementation needs
the repair integration/release gates below before production enablement.

`graph_projection` in revision/SSE notices does not decode body parts. It reports
expected rules 2 even before the first compatible result. A generation ID hashes
dataset + applied checkpoint + rules; source-only movement does not force a full
reload. These notices describe publication state, not content verification.
Every body read still verifies the strict bundle header, exact parts, digests and
repository scope. Runtime generation annotations are never persisted as content.

The serving application rechecks current authorization after resource admission,
then pins document compatibility and bundle reads in a read transaction. Default
graph/full/live reads do not enumerate authoritative snapshots or proofs. An
explicit historical position uses the completed bundle under bulk admission;
a position absent from a lagged bundle is pending rather than falsely absent.
The independently projected On Hold list does not wait for the full graph.

MCP/current-context and effective-memory reads have not switched to this
lag-tolerant contract. They retain their current evidence and ownership checks.
A focused regression enables failed graph serving, then confirms current
context selection and a newer attached memory still use the authoritative path.
That isolation is not a completed durable MCP/memory projection. Sampled
qualification and quantitative mixed-load gates remain open; this is not approval
to turn on production readers. Apply migrations through 0104 before enabling the new
graph worker/reader binary; earlier migrations are not rewritten.

### Bounded shadow qualification (D3g)

`CXT_GRAPH_SHADOW_REPOS` selects exact repositories for background comparison.
It neither enables graph publication nor switches readers. Run it alongside
`CXT_GRAPH_PROJECTION_REPOS`, leaving `CXT_GRAPH_READ_REPOS` unset, to observe
old/new parity without changing foreground availability. Removing a startup
list drains that process only; shared durable enablement is explicit state.

The worker compares a complete current bundle with the direct graph/view path
inside one read snapshot. Different source checkpoints are not mismatches: work
is deferred before loading the full direct inputs. A streaming preflight counts
logical bytes and rows from the actual current metadata/proof tables, not from
the journal whose coverage is being checked. Row, total-byte and row-count limits
apply before direct materialization/projection. The existing shared bulk
admission and 30-second work deadline remain. Repository attempts rotate so one
busy or failing repository cannot monopolize the sampler. Each process starts
at most one comparison per minute; a database lease and per-repository cooldown
also constrain duplicate replicas. This is not a global fleet-wide rate limit.

Migration 0104 persists one bounded work/result row per repository. It records
dataset, source incarnation/sequence, projection/comparison rules, fencing and
canonical expected/actual digests, not conversation content. Comparison covers
the complete typed bundle, including internal merge identity and nil/empty and
ordering distinctions. The digest uses its canonical field-hash manifest; it is
not a substitute for decoding and verifying every selected payload part.

Claim, comparison and completion use separate transactions. Completion requires
the same unexpired lease and ready identity; source movement after the pinned
comparison may retain that receipt only for the identical prior ready result.
Reset, disable, repair, replacement and stale lease holders cannot publish a
qualification for another generation. A mismatch blocks that result without
rewriting source data or starting corruption repair. Restart preserves both
matching and mismatching receipts.
An input-limit failure is also retained as attention-required for that exact
attempt; the unchanged generation is not repeatedly scanned on every cooldown.
Transient read failures remain retryable, and a genuinely new generation may be
checked again.

The D3g experimental reader required a matching exact-generation receipt and
checks its digest against the validated payload. Missing qualification is
`graph_projection_pending`; a semantic mismatch is `graph_projection_failed`.
If a previously matched receipt no longer matches otherwise well-formed output
bytes, the exact completed output is corrupt: the existing bounded derived-only
repair path applies. This does not apply to a mismatching comparison, a malformed
receipt, or a body-free notice.
Cheap notices suppress readiness until the same gate passes but do not attest
payload bytes. Current request authorization and document compatibility are
still independent checks. A compatible qualified lagged result can be read;
one matching sample never certifies future generations.

That gate alone made newly published, unqualified heads unavailable and could
remain pending indefinitely under continuous publication. D3h replaces this
exact-latest dependency for Web readers with the retained-head contract below.

### Retained qualified serving head (D3h)

Migration 0105 introduces one `graph_serving_heads` row and bounded
`graph_serving_parts` per repository, separate from the candidate's parts.
`FinishGraphQualification` commits a matching receipt and its retained bundle
atomically. The candidate is decoded and its manifest compared again under the
work lock in one REPEATABLE READ transaction before promotion. Size checks and
payload reads therefore see the same version. The inserted payload comes from
those checked bytes, not a second SQL read. Serialization conflicts retry rather
than weaken this guarantee. Mismatch leaves the former serving copy and proof intact.

`ReadQualifiedGraphProjection` and its body-free status counterpart use one
read-only snapshot for current source, identity, enablement and retained head.
They neither consult the latest comparison outcome to approve an older head nor
depend on old candidate parts surviving cleanup. Epoch/rules/compatibility,
disabled workers, terminal failures and quarantine are still authoritative.
Journal floor movement alone does not invalidate a complete retained copy.
Only equal applied/source checkpoints are current. Original rows are never
rewritten to make the comparison pass.

Every promotion receives a fence from a dedicated noncycling database sequence,
not from a reconstructible qualification row. Public generation hashes include
that fence. Serving corruption receipts compare the exact head and fence;
stale reports cannot delete a repaired or newly promoted head. Serving repair
clears only its exact matched qualification, invalidates only the retained copy,
and uses the existing three-rebuilds-per-hour budget policy in a separate bounded
ledger. The fourth incident quarantines serving promotion, including workers
already comparing when quarantine was recorded. Promotion uses a locking ledger
read, so an older REPEATABLE READ snapshot cannot miss quarantine committed while
waiting for the work lock. Candidate corruption instead
uses the original candidate repair path after the owned comparison read closes.
Scoped candidate corruption remains retryable even when the failure was an
oversized stored output and the repair request itself fails temporarily. Actual
source/computation limits still suppress unchanged oversized work. If an old head
is incompatible, a failed current comparison reports attention just as it does
when the retained head is physically absent; no old bytes are exposed.

A migration or derived-state rebuild may leave a matched receipt without a
serving head. The claim gate permits a fresh comparison for that exact current
key, then atomically recreates the copy; the prior receipt alone does not serve
anything. Existing semantic mismatches remain suppressed until inputs change.
Both body and notice consume the retained exact proof. Notices still do not
validate payload bytes, and neither query caches a user's authorization.

**Production availability gates remain open.** Retention removes interruptions
caused solely by publishing a newer unqualified candidate. It does not guarantee
initial qualification or latest-source catch-up under perpetual writes. If the
only serving copy is corrupt, this version waits for requalification rather than
restoring from a retained proof without a new comparison. Larger bundle capacity,
mixed-load cost, current-memory/MCP semantics and release verification remain
separate work. Shadow-only observation remains the safe rollout configuration.

### Write-side proof fences

The common source sequence also participates in optimistic validation of a
prepared Git admission or contribution. Adding evidence capture exposed an
ordering dependency: finishing a Git head scan writes observations and receipts,
which now advance that sequence. Comparing a pre-read generation only after
those writes misidentifies the operation's own output as an intervening change.

The reconciliation transaction therefore checks current authorization, origin,
the complete frozen generation and application revisions **before** finishing
the provider page. The existing catalog-row write lock remains held through
receipt creation, context-head binding, admission and match-cursor publication.
Only that validated local snapshot is passed to those known output operations;
there is no re-read of whole history inside the writer. Stale inputs still defer
their derived bindings while the valid provider page and retry cursor can be
saved. Errors still roll back the entire transaction.

Do not replace this fence with only the metadata-journal tail: prepared ancestry
can change without a history/ref image or G/P/E revision. A locally tested
metadata-only alternative passed ordinary positive tests but was discarded on
that counterexample. Current commit evidence remains part of the complete
generation. A negative regression changes persisted commit parents between
preparation and application and requires rejection without side effects;
restoring the evidence requires fresh preparation before a successful retry.

The lock prevents concurrent commits, not arbitrary mutations made by the same
transaction. Preserve the ordered callback's narrow responsibility: validate
inputs, then write only its observations, receipts, bindings and admissions.
Standalone persistence without a prior validated snapshot keeps the full fence
and rejects an already changed generation. This is not permission to subtract
the caller's writes, ignore sequence conflicts or reuse a fenced snapshot after
its transaction ends.

Release ordering matters: before migration 0101, drain head-scan reconcilers that
still finish before validating their generation. Start them only on the ordered
writer. Public read/cursor compatibility is not writer-composition compatibility.
Reader rollback must retain the corrected writer or pause these workers; no
production migration or worker handoff has been performed by this local change.

## D3i: current AI-query source boundary

The graph serving bundle and the current AI query are different consistency
contracts. `QueryContext` and `QueryEffectiveMemory` continue to read current
authoritative state; enabling qualified Web graph reads does not change them.
A retained, matching Web generation can be older than current code or memory.
It must never silently supply one part of a current AI-memory response.

Effective-memory state now includes the current graph-source checkpoint when a
supported adapter positively identifies the enclosing read-only transaction.
The checkpoint, source metadata, history, memory and Git evidence belong to that
same MVCC read. This catches direct evidence commits even when the legacy
graph/pending/evidence counters do not move. Both a new sequence and a new epoch
invalidate earlier cursors and cached complete-assessment proofs.

| Dependency | Current query boundary |
| --- | --- |
| Git trees, commit/delta evidence, PR/Git integration proofs | Current graph-source checkpoint plus existing typed proof validation |
| Origin, selection, accepted history and inclusion | Existing exact selection/history/lineage inputs, not latest Web labels |
| Snapshot memory attachment and graft lineage | Re-read selected lineage and preserve its fingerprint |
| Immutable memory versions and contribution-memory pins | Re-read current selected versions/pins and validate hashes; not captured by graph readiness |
| Memory/component ownership and current bytes | Authoritative storage checks on every request, including warm proof hits |
| Repository authorization and document identity | Existing request/transaction checks; no cached permission grant |

The process cache stores only the complete assessment hash. It still performs
per-request memory validation and page assessment. An adapter without a proven
current read-only source generation, an unavailable checkpoint, or a nested
write cannot read or populate that cache. Integrity, cancellation and unknown
storage errors are not downgraded to a cache hit. A caller-owned write therefore
does not publish a reusable proof for data that can still roll back.

Continuation identity and semantic delivery identity remain separate. The former
may conservatively change after unrelated source activity; the latter describes
only selected knowledge/provenance and must remain stable in that case. Context
segment cursors additionally bind their already-computed semantic delivery hash,
so an origin change cannot join pages with different provenance. They do not
adopt the global source clock, preserving continuation across unrelated captures.

These boundaries are not a durable effective-memory read-model implementation.
A future one needs explicit versioned memory dependencies, ownership/lifetime
checks, retention pins, bounded rebuild/qualification and a declared freshness
contract. Graph-source coverage alone cannot authorize that cutover. Direct
fallback adapters retain their existing continuation semantics, without a claim
of PostgreSQL-style source-generation completeness.

## D3j: memory admission and authority lifetime

Current memory reads share the existing bulk resource queue with workers and
other expensive reads. The six public read methods include the code-position
picker because it computes history/contribution evidence, not only labels.
Owned work waits before opening its read snapshot; borrowed or unknown
transaction ownership never waits. A legacy trusted caller without a current
authority callback receives immediate admission only, unless it is an explicit
system actor.

HTTP and MCP attach current-authority callbacks and retain admission through
response assembly. Credential, repository binding and read authority are checked
after admission; no memory payload read precedes that check. Nested same-service,
same-repository operations reuse a live scope, not a durable permission cache.
The scope expires on return even when the enclosing bulk permit remains live.

This changes resource ownership, not source identity or freshness. Memory and
component ownership/hash verification remain mandatory; a graph source or
retained Web generation does not authorize cached memory. Durable memory
dependencies/retention, qualification and mixed-load limits remain open gates.

## D3k: independent memory metadata generation

`ReadRepositoryMemorySourceGeneration` binds a graph checkpoint and a separate
memory epoch/sequence in one read-only transaction. It is a private invalidation
contract, not an object capture, retention lease or availability certificate.
Source state is created by migration/repository creation, never repaired by a
read. Missing state for an existing repository fails closed.

| Dependency | Invalidation or validation boundary |
| --- | --- |
| Snapshot memory attachments, lineage, history, Git/PR evidence | Existing graph source |
| Legacy memory fields, PR/Git memory pins | Independent memory metadata clock |
| Root/component memory ownership grant, removal or move | Independent memory metadata clock |
| Immutable object bytes or valid representation changes | Current selected-object validation, not a metadata-clock promise |
| Current reader credentials, policy and repository binding | Current authorization after admission, never a generation cache |

Migration 0106 coalesces relevant mutations per transaction, including rollback
and cascades. New ordinary conversation chunks and queue housekeeping do not
advance the memory clock, and memory-only changes do not advance graph state.
Concurrent/newly granted shared objects are why byte fan-out triggers are not
used: an older writer snapshot need not see those grants.

Effective-memory cursor state uses a v3 basis when the metadata capability is
available. A restored ownership grant does not resurrect an earlier cursor
generation. Unchanged delivered knowledge keeps the same semantic hash. Cached
proofs still require successful current root/component ownership and integrity
checks; corruption cannot fall back to old content. FS/legacy adapters continue
live reads without a complete dependency claim.

Install the migration before compatible readers, and drain older readers for a
cursor-consistent cutover. This is not authorization for deployment. A durable
memory model still requires captured dependency lifetimes, retention/pins,
bounded rebuild and qualification, and an explicit current-serving contract.

## D3l: frozen memory input and detached calculation

`CaptureEffectiveMemorySource` is an internal, selection-scoped source capture.
It requires a positively identified read-only transaction and shares bulk
admission; borrowed writes and unsupported adapters cannot produce a capture.
Graph/evidence, metadata generation and exact publication pins come from one
MVCC snapshot. Graph inputs can use the existing baseline/delta reconstruction.
The existing memory selector records only demanded, hash-validated logical
objects, with separate observed-absent entries. A lookup missing from the
completed capture is an integrity error, not absence and never a live fallback.

The PostgreSQL capture ports separately bound ownership-scoped object bytes and
pin rows before loading full payloads. A digest's root and distinct components
share a cumulative stored-payload allowance, including valid compressed padding;
SQL withholds the next payload if it exceeds the remaining allowance. This is
separate from streaming decompression and logical occurrence accounting, which
prevent repeated manifest hashes from expanding past the assembly budget.
Canonical hashing also stops before allocating an oversized escaped JSON
representation. Returned nested data is independently owned. This
does not replace or weaken normal `GetMemory` ownership/hash validation.

`CapturedEffectiveMemory.Project` is a deterministic internal replay, not a
read endpoint. It has no general metadata/blob adapter, preserves the v3 source
and semantic delivery identities and only permits continuation/page-size changes
within the captured selection/content mode. Full frozen evidence is available
for later pages even when a delivery proof was omitted by its work budget.
An independent missing-data observation remains historical; it is not a claim
that current storage is missing the object.

The D3l capture itself has no serving path. D3m adds the restartable input
representation below. Original object/component lifetimes, durable qualification,
current authority/freshness checks and mixed-load/RSS acceptance remain required. A valid metadata generation
does not substitute for these contracts. Failed capture returns no complete
input, and a successfully captured input is not an approved output generation.

## D3m: restartable frozen memory inputs

`MemoryCaptureBundle` binds the selector, source generations, content mode and
ordered graph/object/pin parts with a versioned streaming content digest. Logical
memory objects use 64KiB parts; explicit `absent` records differ from missing or
empty object parts. Restore verifies canonical object identities and reruns the
shared selector against frozen inputs to check dependency closure.

Migration 0107 stores repository-scoped deduplicated blocks, part references and
the complete header in one transaction. Loading in another process verifies
stored blocks and the root. Retrying a live identifier never extends expiry or
repairs conflicting bytes. Expiry uses database time; bounded pruning removes
only expired bundle references and otherwise-unreferenced captured blocks.
Original objects, grants and source journals are untouched.

This is a historical input format, not a current result, access credential or
retention certificate for the original objects. No endpoint uses it yet.
Block deduplication does not make exporting all graph part references per change
incremental: current-memory preparation must reuse shared generations and
dependency changes before it becomes an always-on worker. Current authority,
freshness, output qualification and operational lifetime policy remain open.

## D3n: persisted memory results

`BuildMemoryRead` shares the existing selection and item assessment rules but
uses strict preparation budgets. Missing captured evidence remains reviewable;
Git/natural ancestry, receipt scans or branch selection stopped by a work limit
return no candidate. A private preparation mode reaches nested evidence readers;
it does not alter the current endpoint's bounded-review behavior.

`MemoryReadBundle` binds the capture, source generation, selector, content mode,
dependency observations, continuation/delivery identities and ordered chunk
references. Migration 0108 atomically stores immutable headers and indexed
payloads tied to a live 0107 input. Duplicate writes verify rather than repair;
expiry/pruning cascades to derived results without extending input retention.

Internal range reads verify the header identity and selected chunk hashes,
preserve v1 cursor/item identities and page byte limits, and never reconstruct
the source graph or decode unrelated payloads. This is not current authorization.
Current endpoints still require current object ownership and remain unchanged.

The point-read adapter retains full-index validation for audit. D3o below removes
that repeated work from internal page assembly. Incremental preparation,
qualification, current authority/lifetime and rollout contracts remain required.

## D3o: scoped result range reads

`MemoryReadRangeStore.WithMemoryReadRange` supplies one validated, owned header
and a scoped `MemoryReadChunkReader`. The application chooses the cursor and
page; PostgreSQL owns the repeatable-read snapshot, repository/result binding,
selected row metadata/hash checks, strict payload decode and final database-time
expiry check. The header's stored and canonical hashes must match the result ID.
The private verifier owns copied chunk references, so callback mutation cannot
change the meaning of later reads. It does not repeat header validation per chunk.

The callback receives independently owned typed items, not raw bytes to decode
again. A reader cannot be reused after the callback; failures cannot be swallowed
to resume a partially successful operation. Scope cancellation propagates into
in-flight SQL even through `context.WithoutCancel`. At most 51 reads and three
maximum chunk bodies are allowed, including any skipped prefix or unconsumed
tail. A terminal failure returns no application page.

Publication's sealed/deferred complete-index constraints are unchanged. Range
reads deliberately do not enumerate unrelated index rows or detect their runtime
corruption; full-index/point and full-bundle audits remain available. A coherent
historical snapshot can survive concurrent pruning, but it cannot survive its
wall-clock expiry or grant current access to deleted/revoked source objects.
The public endpoints are not switched to this historical reader.

For two selected chunks, a 640→6,400-item PostgreSQL fixture keeps five SELECTs
and five returned rows, versus 14 SELECTs and 41→311 rows with point operations.
Measured wire bytes fall, but the remaining header grows 2,176→13,061 bytes.
This is bounded range-work improvement, not an O(1) or production-SLA claim.

## D3p: selected memory object witnesses

Repository metadata generations do not certify original memory bytes. A verified
capture now retains root/component grants, per-blob incarnation and existing
`doc_range_source_version` in the same read snapshot as the validated payload.
Explicit missing roots are observations too. Migration 0109 adds only keyed
incarnations; it does not duplicate the write counter or update the payload heap.
The existing version always advances on writes; deletion/reinsertion creates a
new incarnation. Side-metadata loss fails closed rather than silently repairing
a witness. Administrator trigger bypass still needs an integrity audit.

`MemoryObjectWitnessSet` owns and deduplicates physical dependencies across all
selected memories, enforcing one root-inclusive 4,096-object limit for both
capture and validation. Conflicting versions or exceeded bounds poison the set;
no partially accepted closure can be used. Current application validation rejects
borrowed/unknown transactions, reauthorizes through normal resource admission and
opens a fresh read snapshot for graph/memory generation and object metadata checks.

The selected metadata check uses 128-object SQL batches and no payload transfer.
Five physical objects use one SELECT/five rows/380 received bytes at both 100 and
1,000 unrelated owned objects in the local PostgreSQL fixture. This is only the
validation cost; capture, output preparation and service overhead are separate.
Historical input export/restore does not persist these witnesses and cannot
claim current validity. A successful check is not a lifetime lock or a durable
publication certificate. Qualified-result binding, source retention/fencing and
incremental workers remain prerequisites for current API/MCP cutover.

## D3q: worker-qualified memory ranges

Migration 0110 adds immutable content-addressed qualifications. Each binds a
persisted result/capture pair, exact source generation and the selected verified
object witnesses. Preparation/publication are internal system-only application
operations; untrusted outputs cannot be supplied through that application API.
A qualification is a derivation certificate, never an access credential.

Final publication owns a fresh read-committed transaction. It uses the capture
retention advisory gate and nonwaiting source/header/grant/blob/incarnation row
fences. An inverted writer lock order rolls back rather than waits in a cycle.
The receipt commits only with live input retention; physical rewrites require a
new witnessed qualification even when logical input/result hashes are unchanged.
Receipt JSON is bounded cumulatively before full serialization, including shared
component occurrences rather than only unique objects.

The internal reader rejects borrowed transactions, opens a fresh readonly RR,
invokes the explicitly supplied snapshot-safe authorization callback and retains
its context. It then validates the receipt/result association, source generations
and object metadata before consuming selected verified output chunks. Each
callback must validate credentials/policy using that transaction, without writes
or detached permission reads. Missing callback support fails closed for ordinary
actors; an admission callback is not a substitute. Raw IDP login/session migration
and break-glass auditing in the existing HTTP/MCP callbacks are not snapshot-safe.
Those transports remain on their existing paths pending D3r integration.

Expiry/cancellation after consumption fails the whole operation. Concurrent
pruning or source revocation does not fracture a coherent in-flight snapshot;
the next snapshot refuses missing/changed sources. This is snapshot-consistent
access, not through-response credential locking. Qualification publication is
derived worker work and does not depend on the initiating user's session staying
alive. Current public serving still requires worker leases, incremental
preparation and explicit transport authorization/rollout acceptance.

Local PG tests verify restart, rollback, contention, deterministic source-write
interleavings, final-commit expiry and selected range costs. One selected result
chunk uses nine SELECTs/nine rows/4,253 received bytes with 100 or 1,000 unrelated
owned objects. These figures exclude transport auth and cold preparation; larger
headers/dependency closures are still bounded work, not constant cost.
