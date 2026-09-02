import assert from 'node:assert/strict';
import { layoutGraph, orderGraphSnapshots } from '../src/graph.ts';
import type { Snapshot } from '../src/types.ts';

function snapshot(
  id: string,
  createdAt: string,
  parents: string[] = [],
  graftParents: string[] = [],
): Snapshot {
  return {
    id,
    repo_id: 'repo',
    branch: 'main',
    parents,
    graft_parents: graftParents,
    grafted: graftParents.length > 0,
    doc_hash: id,
    provider: 'codex',
    fidelity: 'full',
    message: id,
    created_at: createdAt,
  };
}

const root = 'root';
const newerGraftParent = 'newer-graft-parent';
const olderGraftedChild = 'older-grafted-child';
const independentRoot = 'independent-root';
const independentHead = 'independent-head';

// This is the production failure shape: the immutable child was created first,
// then later grafted to a newer parent. A plain created_at DESC list puts the
// parent before the child and layoutGraph cannot draw backward to an old row.
const reverseTimeGraft: Snapshot[] = [
  snapshot(independentHead, '2026-08-30T05:00:00Z', [independentRoot]),
  snapshot(newerGraftParent, '2026-08-30T04:00:00Z', [root]),
  snapshot(olderGraftedChild, '2026-08-30T02:00:00Z', [root], [newerGraftParent]),
  snapshot(independentRoot, '2026-08-30T01:00:00Z'),
  snapshot(root, '2026-08-30T00:00:00Z'),
];

const ordered = orderGraphSnapshots(reverseTimeGraft);
assert.deepEqual(
  ordered.map((item) => item.id),
  [independentHead, olderGraftedChild, newerGraftParent, independentRoot, root],
  'child-parent constraints win over time, while simultaneously ready rows stay newest-first',
);

const position = new Map(ordered.map((item, index) => [item.id, index]));
for (const child of ordered) {
  for (const parent of [...(child.parents ?? []), ...(child.graft_parents ?? [])]) {
    if (!position.has(parent)) continue;
    assert.ok(
      (position.get(child.id) as number) < (position.get(parent) as number),
      `${child.id} must render before ${parent}`,
    );
  }
}

const layout = layoutGraph(reverseTimeGraft, independentHead);
const childRow = layout.rows.findIndex((row) => row.snap.id === olderGraftedChild);
const parentRow = layout.rows.findIndex((row) => row.snap.id === newerGraftParent);
assert.ok(childRow < parentRow, 'layoutGraph must apply graph ordering at its single entry point');
assert.equal(
  layout.rows[parentRow].incoming.includes(newerGraftParent),
  true,
  'the graft parent row must receive the lane opened by its older child',
);

const sameTime = '2026-08-30T06:00:00Z';
assert.deepEqual(
  orderGraphSnapshots([
    snapshot('first', sameTime),
    snapshot('second', sameTime),
  ]).map((item) => item.id),
  ['first', 'second'],
  'equal-time unconstrained rows retain input order',
);

const cycle = [
  snapshot('cycle-new', '2026-08-30T08:00:00Z', ['cycle-old']),
  snapshot('cycle-old', '2026-08-30T07:00:00Z', ['cycle-new']),
];
assert.deepEqual(
  orderGraphSnapshots(cycle).map((item) => item.id),
  ['cycle-new', 'cycle-old'],
  'legacy/corrupt cycles retain every row in deterministic recency order',
);
