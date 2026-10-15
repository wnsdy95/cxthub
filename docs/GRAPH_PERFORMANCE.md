# Graph performance and defensive input checks

Run from `frontend/web`:

```sh
npm test
npm run benchmark:graph
CXT_E2E_FULLSTACK=1 npm run test:e2e
```

The benchmark builds deterministic chain, PR, wide-lane and overlapping-rewind
fixtures. It reports the median of three runs on the local Node process. It
measures index/evidence/group construction, operation projection, lane layout,
and eight visibility changes separately. Visibility changes must perform zero
ancestry queries. Cached closures must remain within the membership budget.
No machine-specific millisecond threshold is used as a CI correctness test.

## Measured example (2026-09-18, macOS, Node 22)

| Fixture | Index, evidence and groups | Projection | Lane layout | Eight visibility changes |
|---|---:|---:|---:|---:|
| 10,000-node chain | 8.2 ms | 3.1 ms | 12.5 ms | 12.3 ms |
| 10,000 nodes + 300 PRs | 10.4 ms | 14.1 ms | 18.0 ms | 14.1 ms |
| 10,001 nodes / 100 lanes | 4.9 ms | 1.7 ms | 23.5 ms | 11.6 ms |
| 10,000 nodes / 100 overlapping rewinds | 149.4 ms | 2.3 ms | 10.6 ms | 12.0 ms |

An identical one-run before/after harness measured the 300-PR projection at
4,559 ms before indexing and 24 ms afterwards; evidence + projection + layout
changed from 4,574 ms to 58 ms. The previous implementation repeatedly built
snapshot maps, scanned all history for each node and scanned every node for
each merge. The replacement shares an index and looks up only children whose
parents can actually be redirected by that merge.

These are CPU observations, not browser paint or network latency guarantees.
Very wide graphs still produce row-by-lane layout output. Many overlapping
retention groups can contain the same IDs: the cache bound does not bound the
size of the input, group results or layout output. Browser windowing now bounds
mounted graph rows as described below; remote pagination and real cloud
load/failover measurements remain separate work. A repeated view object is
cached by React; a new coherent server view
invalidates the index and evidence together.

## Correctness guards

- Exhaustive 64-subset visibility fixture: unchanged full-view edges, no
  invented bridges over hidden real captures, no ancestry queries on folds.
- Browser traversal of all eight archive/two-overlapping-progress combinations;
  verified PR evidence and actual rendered SVG paths remain correct.
- Duplicate IDs, self-loops, natural-parent cycles and graft cycles. Invalid
  hidden inputs are rejected before visibility is applied. A partial view with
  missing parents remains distinguishable from a cycle.
- Browser invalid-response → diagnostic → readable context → valid response →
  recovered graph, with no write requests.
- 50,000-node chain, bounded closure cache and deterministic generated DAGs
  compared against exact traversal, including cross edges and reversed inputs.

## Server projection boundary (2026-09-20)

The table above records the earlier frontend implementation. Business evidence,
retained progress and publication now run in Go. `npm run benchmark:graph` invokes
the real server fixture once per dataset, decodes its HTTP representation, then
measures browser projection/layout/folding separately. `serverProcessMs` includes
process startup, JSON input/output and projection; it is not database latency.

Reproduce the domain benchmark from the repository root:

```sh
go -C backend test ./internal/domain -run '^$' -bench BenchmarkGraphState -benchmem
```

On the development machine, 10,000 snapshots with 100 completed branch merges
required about 64 ms and 66 MB of total allocations per query, compared with
330 ms and 228 MB before replacing repeated ancestry hash maps with indexed
bitsets. A straight 10,000-snapshot chain took about 28 ms. Total allocations
include the result and metadata index; the 8 MiB ancestry cache cap does not
bound the size of the repository or returned groups.

A 677-snapshot, 55-merge metadata sample retained the same operations while the
graph portion fell from 1,321,475 bytes to 311,121 bytes with dictionary encoding;
gzip at the fastest level reduced it to 78,017 bytes. These are payload/CPU
measurements, not cloud throughput or browser paint guarantees. Neither this
transport nor the cache changes stored ancestry, history, memory, or access rules.

## Branch timeline transport and projection reuse (2026-09-23)

The web client negotiates `graph_encoding=indexed-v2` on view, pending-view and
graph-state. Older clients continue receiving indexed-v1. V2 encodes branch roots
as dictionary indices and references branch_snapshots when the ordered timeline
is identical. Independent timelines remain representable. A Go-to-TypeScript
contract test checks full semantic equality, invalid references and input ownership.

On the same saved 1,043-snapshot / 40-branch response, compact JSON pending-view
fell from 2,181,913 to 1,056,980 bytes. Gzip level 1 fell from 701,885 to 171,175
bytes (76% reduction). Full view fell from 4,208,352 to 3,083,419 bytes, or
1,004,216 to 470,157 bytes with gzip level 1. These compare serialization of the
same data, not separate live generations or cloud response times.

The application caches committed branch inclusion by repository, graph revision
and evidence revision. Pending-only changes reuse this projection but recompute
current display integration against the fresh coherent view. Historical position
queries and in-transaction writes bypass it. Copies prevent response mutation
from changing cached facts. The process-local cache keeps at most eight repositories
and 250,000 weighted identifiers; oversized projections are not cached. It stores
neither permissions nor document bodies. Full metadata reads and base domain
projection still occur; this is not database pagination or an O(1) pending query.

## Browser row window (2026-09-29)

Graphs above 80 rows mount the visible rows plus six rows of overscan on each
side. The focused row and native drag source stay mounted until the interaction
ends. The complete server projection, validation, folded-edge evidence, lane
layout and per-segment labels remain available; windowing does not infer or
remove a branch, merge, snapshot or permission.

Prefix offsets include the 20px unpushed dividers as well as 26px node rows.
Binary search finds the visible interval; one scroll listener and one resize
observer replace an intersection observer for every node. Labels use the visible
interval rather than overscan or pinned offscreen rows. Horizontal scrolling
still moves labels and SVG lines in the same canvas.

External selection reveals its row. Arrow keys, Home/End and Page Up/Down can
focus records outside the mounted window; Enter retains normal button selection.
Live insertion preserves the first visible row by identity and pixel offset,
while readers at the top see new work. A replaced focused observation transfers
focus to a surviving row. Small graphs keep their existing complete DOM.

Regression evidence includes exact visible intervals across 10,000 variable
height rows and browser tests with 1,000 snapshots. The latter retain fewer than
32 mounted rows at the standard viewport, select an offscreen search result,
check SVG paths across the window/divider, preserve scroll position on live
insertion and traverse historical PR operations and long branch labels. Existing
folding, invalid-input, archived-path and horizontal-scroll regressions remain.
This bounds browser DOM work; it is not server pagination or a cloud latency
claim, and extremely wide lane layouts still consume memory proportional to
their complete layout.

## Graph controls (2026-10-14)

The graph toolbar switches between Commits (the default) and PRs, with zoom
from 50% to 200% in 25-point steps and a percentage button to reset to 100%.
Zoom scales SVG geometry, row offsets, dividers and sticky labels together.
The viewport retains its row-relative vertical position and horizontal scale;
switching view modes reveals the selected record without changing the reader's
selected context. Repository changes reset these presentation preferences.

PR view consumes the same server-confirmed projection. It folds only linear
single-parent, single-child capture chains. Recorded branch births and PR
merges, branch/ref tips, graph junctions, uncommitted records and a selected
commit remain visible. PR numbers identify merge nodes. Selecting a PR still
opens its original source snapshot, not a newly assembled document.

This is display contraction, not a new ancestry or merge decision. It never
crosses missing or archived/hidden captures; lifecycle and graft endpoints
remain intact. The hint and node tooltip report folded records. Join gestures
are available only in Commits view, where all visible records are explicit.
Changing views makes no writes and does not alter publication counts, saved
parents, PR completion, memory or context inclusion.

Tests cover a 100,000-node linear contraction, unchanged PR evidence and source
selection, missing/hidden inputs, graft and lifecycle edges, invalid structures,
zoom limits, fractional scroll anchors and rendered SVG continuity across
scaled dividers. The PR view reduces layout rows, not the initial API payload
or database read cost.

## Shared backend admission and projection reuse (2026-10-14)

`app.AcquireResources(ctx, class)` returns a permit-bearing context, a release
function and an error. Use that context for synchronous nested work and release
on every exit; do not share a permit across independent concurrent operations.
Admission precedes database snapshots, worker claims and HTTP upload decoding.
One process admits at most two heavy operations, at most one of them bulk.
Graph/full-view/legacy pending reads enter as interactive; a cache miss or cache
bypass must upgrade to bulk before graph computation. Warm results can therefore
be read while a bulk worker runs. Upload verification, history preparation,
document/PR/Git jobs and Git reconciliation share the same bulk capacity.
Context-query and the independent On Hold projection use interactive capacity.
Revision reads and the existing coalesced SSE polling remain outside this gate;
document range reads select capacity at their own preparation/read boundaries.

Legacy whole-document reads (full document, strict events without `view`, search,
fragments, agent-history pages, downloads and diff) acquire bulk before storage.
HTTP full-document, strict-events, turns, search, pull-objects, pull-chunks and
diff handlers retain the same context permit through response serialization and
body writes, including write-error exits; nested application reads cannot release
the outer HTTP permit. New bounded document view/fragment endpoints are not
wrapped in bulk for root documents with verified range indexes. If a view page,
fragment, or inherited-prefix comparison reads a legacy document, its permit
upgrades to bulk before the whole-document index is loaded and remains charged
until the outer page/fragment read finishes. MCP/direct application callers receive application-level
admission only: the caller's later serialization/retention is not covered unless
it holds an outer permit itself.

Each class queues at most eight callers for at most one second. Saturation or
an expired admission wait returns `resource_overloaded` (HTTP 503 with
`Retry-After: 1`); caller cancellation preserves its context error. A nested
bulk permit can serve interactive work. Interactive-to-bulk escalation succeeds
only immediately when bulk capacity is free and no bulk caller is queued;
otherwise it returns overload, never waits while holding the interactive slot.
An upgrade remains charged until the outer release. Workers acquire before
claiming durable work, so admission rejection does not increase job attempts.
The existing durable retry/lease policies are unchanged. A missing document
range index also maps to retryable HTTP 503, `doc_range_index_unavailable`.

Graph-state, full-view and pending-view reuse the same immutable graph result
only after fresh coherent metadata/compatibility reads. Keys include repository,
graph/pending/evidence revisions, position, Git origin and a fingerprint of all
view inputs: uploads can change snapshots without advancing a public revision.
Each service cache retains at most four entries and 8 MiB of encoded projections; decoding
owns each response's mutable collections. An identical concurrent cache miss
returns retryable overload rather than waiting while holding another read slot.
Zero revisions, writes and adapters without a positively identified read-only
snapshot bypass this cache; PostgreSQL can reuse it, FS deliberately recomputes.
Authorization remains at the existing transport/service boundaries. No native
documents or permissions are cached, and no new per-row queries are introduced.

`GET /pending-view?projection=on-hold-v1` explicitly selects `PendingStateView`:
`projection`, `revision`, `default_branch`, `refs`, `pending`, `snapshots`,
`archived_sessions`, `hold`, `orphan_sessions` and `hold_counts`. It has **no
`graph` field** and must never replace a complete graph. Missing/empty projection
keeps the original full `PendingView.Graph` contract; unknown, malformed or
duplicate projection parameters are rejected. Both paths retain viewer
authorization and document-identity compatibility checks.

The lightweight projection shares the domain's hold/orphan classifier (including
historical publications, completed PRs, reflog roots, grafts and dismissals), not
application branch integrations, Git evidence or all-branch query construction.
Its snapshot metadata covers pending targets, held captures and archive sources,
without owning full graph branch memberships. Each read uses fresh repository
graph/pending/evidence generations; PostgreSQL pins the metadata together, while
FS retains its existing read consistency. It still scans repository metadata,
so it is independent of full graph computation, not constant-cost pagination.

The local mixed regression runs 24 operations (eight graph, eight interactive
graph, eight canonical document verifications), with 10,000 snapshots and a
1,069,107-byte document. On macOS with `GOMEMLIMIT=512MiB`, the final test-binary
run completed in 3.09s wall time (1.33s workload), with maximum RSS 102,039,552
bytes (97.3 MiB), 1,004 explicit overload retries and a 3,700,647-byte graph cache.
Other local test processes were running concurrently; retry counts and timing
are observations, not latency guarantees. Reproduce from `backend`:

```sh
go test -c -o /tmp/cxt-admission.test ./internal/app
/usr/bin/time -l env GOMEMLIMIT=512MiB /tmp/cxt-admission.test -test.run '^TestResourceAdmissionMixedGraphVerification$' -test.v
```

This uses synthetic metadata, not HTTP/PostgreSQL/end-to-end production load.
The Go memory setting is a soft runtime target, not an RSS cap. Full metadata
reads, individual graph size, response encoding, DB/native memory and other
maintenance paths still require separate production-sized measurement; admission
and cache retention alone do not establish a 512 MiB deployment guarantee. Even
one whole-document read plus JSON encoding can exceed that target. A concurrent
race-instrumented run with the same memory target hit this synthetic test's
30-second deadline; race overhead is not represented by the RSS figure above.
The focused race suite, including that mixed test, subsequently passed without
the memory target. This does not establish a race-instrumented 512 MiB guarantee.
