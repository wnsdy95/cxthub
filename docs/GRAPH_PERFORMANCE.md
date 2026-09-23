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
size of the input, group results, layout output or DOM. Browser virtualization,
remote pagination and real cloud load/failover measurements remain separate
work. A repeated view object is cached by React; a new coherent server view
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
