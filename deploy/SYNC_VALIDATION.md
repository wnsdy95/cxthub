# Large context synchronization: predeployment validation

Scope: steps 1 and 2 of the deployment preparation, completed locally on
2026-09-28. Target topology is Vercel routing to independent Render API and MCP
services backed by shared PostgreSQL. No cloud resources were provisioned.

## Plan and acceptance criteria

1. Trace upload acceptance, background verification, publication, manifest
   reads and bounded chunk downloads separately. Compare FS and PostgreSQL.
   A successful retry alone is not evidence of fixing the first failure.
2. Fix reproduced defects without raising HTTP/hook deadlines or weakening
   content hashes, repository authorization or transactional publication.
3. Verify durable chunks and job identities survive API restart. Reject
   snapshots whose documents are unfinished, and leave refs unchanged until
   the separate validated publication path completes.
4. Run real CLI/Git-hook E2E against PostgreSQL. Exercise independent API/MCP
   executables through the browser proxy and production Docker images.
5. Measure large documents and concurrent REST/MCP reads. Stop API and database
   processes, verify readiness failure and recovery, and preserve existing data.
6. Review compatibility, run affected suites, and retain the production-path
   regressions in CI. Treat real cloud/provider checks as a separate launch gate.

## Causes and changes

The normal pull application service uses a repeatable-read, read-only
transaction. PostgreSQL manifest retrieval previously executed `FOR UPDATE`
even for an existing manifest. PostgreSQL rejected that read with SQLSTATE
25006. Existing manifests now use an ordinary repository-scoped MVCC read;
they also remain readable when a concurrent publisher holds the row lock.
Legacy full documents use the existing body fallback inside the same read
snapshot. Opportunistic repacking is retained only in writable calls.

Chunk planning previously decoded and copied every event, joined the whole
transcript, copied chunks, and reassembled the document for comparison. V2 now
validates the outer framing and events array, then copies bounded byte slices.
Assembly writes chunks directly into the destination buffer. Canonicalization
normalizes the envelope and each event separately instead of building a second
generic representation of the entire document. Backend and CLI use the same
rules, with the previous algorithm retained as a test oracle.

Snapshot/document identities, stable sequence ordering, number precision,
nested replacements, V1 compatibility, authorization and transaction boundaries
are unchanged. Framing/ownership tests and fuzz tests cover identity parity and
malformed input. These optimizations reduce allocation pressure; they do not
turn full document validation into a constant-memory operation.

The original development hook timeout is not explained by the PostgreSQL
defect: that daemon used FS storage. Background finalization and authenticated
local validation receipts already provide resumable progress. This change
reduces their memory cost and verifies recovery, without claiming every large
backlog can finish inside one bounded Git hook invocation.

## Measurements

Local Apple M5 Pro, PostgreSQL 16; illustrative observations, not cloud SLAs.
Load and lifecycle fixtures are synthetic. The frozen 163 MiB sample was read
locally without publishing its contents or modifying its records.

| Scenario | Result |
|---|---|
| V2 planning, 16 MiB event | Allocated bytes per operation fell from about 80 MiB to 16 MiB; current run about 41 ms |
| Full validation, 163 MiB document, separate process | Peak RSS about 1.45 GiB to 1.08 GiB; elapsed about 4.4 to 4.5 s |
| 32 MiB upload, 65 chunks | API restart after first chunk and after accepted job; same job ID on replay; no premature snapshot/ref publication; final hash verified |
| 32 MiB lifecycle, warm local database | Acceptance about 4.5 ms; background processing about 670 ms; slowest concurrent status request about 17 ms |
| 100 MiB / 4,096 events / 1,001 snapshots | 16 readers, 320 successful requests through two proxies to two API and two MCP processes |
| REST document pages | 160 requests; p95 about 28 ms; largest response about 257 KB |
| MCP queries | 160 requests; p95 about 24 ms; largest response about 13 KB |

The 163 MiB CPU time did not materially improve. The memory reduction matters
for concurrent work, but peak RSS remains above a 512 MB service budget. Keep
the initial API/MCP sizes in the Blueprint and measure staging traffic before
making sizing changes. Pagination latency is not a full-document download rate.

## Reproduce

Use **empty disposable databases**, one per command group below. Tests write
fixtures and retain them; never use a development or production dataset DSN.

```bash
# Real PostgreSQL CLI, provider fixtures and Git hooks (default is FS).
CXT_E2E_DSN='<empty sync-test DSN>' make e2e-sync

# Bounded upload, durable acceptance, restarts and concurrent status reads.
CXT_LOAD_DSN='<empty pipeline-test DSN>' \
  go test -C backend -tags postgres ./internal/adapters/delivery/http \
  -run '^TestPostgresDocumentPipeline$' -count=1 -v

# Independent OS processes, shared DB, simultaneous REST/MCP reads.
CXT_LOAD_DSN='<empty load-test DSN>' CXT_LOAD_MULTIPROCESS=1 \
  go test -C backend -tags postgres ./internal/adapters/delivery/http \
  -run '^TestPostgresMultiInstanceLoad$' -count=1 -v

# Document finalization plus same-repository page reads and ref writes.
CXT_LOAD_DSN='<empty contention-test DSN>' \
  go test -C backend -tags postgres ./internal/adapters/delivery/http \
  -run '^TestPostgresDocPublicationContention$' -count=1 -v -timeout=10m

# PostgreSQL transaction and publication regressions.
CXT_TEST_DSN='<empty regression-test DSN>' \
  go test -C backend -tags postgres -race -p 1 ./...

# Real API/MCP executables, Vite proxy, OAuth consent/PKCE/revocation.
cd frontend/web
CXT_E2E_FULLSTACK=1 CXT_E2E_DSN='<empty browser-test DSN>' npm run test:e2e
```

See [Render deployment](RENDER.md) for `deploy-preflight.sh full` prerequisites.
It builds both images and tests readiness, route isolation, missing-DSN startup
failure, MCP availability during API shutdown, and database outage/recovery.
Docker's ephemeral host port is re-read after restarting API. CI runs image
smoke tests plus the PostgreSQL CLI and document pipeline regressions.

Backend and CLI full test/vet suites, PostgreSQL race tests, four focused fuzz
runs, frontend unit/i18n/routing/type/build checks, and the browser MCP OAuth
flow passed locally. A browser SDK login with real Firebase or GitHub accounts
was not exercised; local auth fixtures are deliberately isolated.

### Local remeasurement, 2026-09-30

At `269a974`, three fresh-database runs each of the existing pipeline and
multi-process workloads passed. The 32 MiB pipeline preserved the document
hash, durable job identity across restarts, and object-before-ref publication.
Acceptance took 2.90–3.12 ms; worker duration was 1.65–1.87 s; the slowest
status read in each run was 7.24–8.06 ms. These are loopback observations on a
shared development machine, not service limits.

The multi-process workload issued 960 successful requests across the three
runs (480 REST and 480 MCP). Per-run REST p95 ranged from 25.50 to 54.04 ms;
MCP p95 ranged from 24.98 to 29.81 ms. Responses stayed below 258 KB and 14 KB,
respectively. Do not average these percentiles into a pooled p95. This fixture
uses repetitive ASCII text and a single tenant, so it does not measure full
document download throughput, realistic payload entropy, or cloud capacity.

The opt-in contention workload complements those read-heavy checks with
mixed Korean/code events and same-repository ref mutations while finalization
is in flight. Its telemetry reports observed overlap, successful sample
counts and integrity separately from latency. Request/worker overlap alone
does not prove a request waited for a database lock. Exact lock-hold time,
cloud network latency, long-running multi-tenant traffic and actual model
200K/800K acceptance remain separate release gates.

Three fresh-database runs of the reviewed contention fixture passed hash,
full-document HTTP readback, page, snapshot, idempotent receipt and exact ref
name/count checks. Each used 34,208,892 canonical bytes, 45,618,630 chunk-upload
JSON bytes, 66 chunks and 1,024 unique events. Each workload had measured
client-request overlap with the publication call; no worker gate manufactured
that overlap. The largest ref-write latency per run was 2.37–2.58 s, although
its per-run p95 was only 2.29–3.11 ms. Read maximums stayed below 23 ms. The
percentile alone hides the write tail and is not sufficient acceptance evidence.
These three measurements used 1 ms pacing. The final fixture uses 10 ms pacing
to keep traffic active under race instrumentation; its three further runs passed
with ref maximums of 1.97–4.53 s. The first overlapped a local HTTP test suite,
so this range is not an isolated before/after comparison. A separate focused
race run also passed. Request ceilings and HTTP response bounds remain enforced.

A separate diagnostic run sampled `pg_stat_activity` in its disposable
database: ref writes waited on an advisory lock while the blocking publication
transaction inserted new `doc_search_events_v2` rows, including temporary-file
I/O samples. Some blocked samples preceded that INSERT while the holder was
idle in transaction during client-side index construction. This identifies a
remaining publication cost; it is not a precision measurement of lock duration
or a claim that moving all work outside the transaction is safe. Any follow-up
must retain current-byte validation, authorization, lease/GC protection and
atomic ownership/index/receipt publication.

## Collection during an in-flight push

The subsequent live-data run exposed another failure after about 24 minutes:
a document advertised as present at negotiation was collected by a concurrent
live-capture replacement before snapshot batch 26 of 31 was published. The
server rejected that batch with 422 and the client sent no ref updates. This
was a failed synchronization, not a successful latency measurement.

Negotiation does not provide a retention lease. The CLI now rechecks the full
original offered object graph when snapshot publication fails. It retries only
when that recheck proves a document or a previously-present snapshot has gone
missing. Only missing bodies are reopened, and existing chunk/job identities
are reused. The server already requeues a completed finalization job if its
body has been collected. Recovery is limited to two attempts and respects
caller cancellation and the ordinary request deadlines. Unrelated metadata
validation errors and body upload errors do not trigger blind retries. Refs,
pending pointers and unsync pointers remain after successful prerequisites.

Synthetic regressions cover lost documents, lost parents outside the initial
upload set, snapshot-only collection, repeated collection, cancellation and
unrelated validation failures. The same recovery path serves ordinary push,
pending synchronization and unsync preparation. This is bounded recovery, not
an indefinite server-side object retention promise.

## Launch gate after deployment

Local proxies and containers cannot establish Vercel gateway behavior, Render
network/cold-start latency, real TLS cookies or external callback settings.
After service URLs are assigned, validate large chunk transfers and retry,
repository streams, OAuth discovery/paging/revocation and real login through the
public HTTPS origin. Measure cold and sustained concurrent traffic and size the
shared database and both service pools together. No cloud capacity claim is
made by these results.
