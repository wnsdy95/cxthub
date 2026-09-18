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
