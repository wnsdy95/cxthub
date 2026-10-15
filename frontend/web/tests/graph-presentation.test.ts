import assert from 'node:assert/strict';
import { summarizePRGraph } from '../src/graphPresentation';
import { projectBranchGraph, visibleBranchGraph, type GraphEvent } from '../src/graphProjection';
import { GraphIndex } from '../src/graphIndex';
import { layoutGraph } from '../src/graph';
import type { Snapshot } from '../src/types';

const snapshot = (id: string, parents: string[] = [], extra: Partial<Snapshot> = {}): Snapshot => ({
  id, parents, branch: 'main', repo_id: 'repo', doc_hash: id, provider: 'codex', fidelity: 'full',
  created_at: '2026-10-14T00:00:00Z', ...extra,
});
const snapshots = [
  snapshot('head', ['merge']), snapshot('merge', ['base', 'tip']), snapshot('base', ['root']),
  snapshot('tip', ['middle'], { branch: 'feature' }), snapshot('middle', ['birth'], { branch: 'feature' }),
  snapshot('birth', ['root'], { branch: 'feature' }), snapshot('root'),
];
const refs = [{ repo_id: 'repo', kind: 'branch' as const, name: 'main', target: 'head' }];
const events: GraphEvent[] = [
  { id: 'merge', kind: 'merge', branch: 'main', sourceBranch: 'feature', snapshot: 'tip', evidence: 'receipt', prNumber: 42 },
  { id: 'birth', kind: 'birth', branch: 'feature', snapshot: 'root', evidence: 'birth-receipt' },
];
const projection = visibleBranchGraph({
  ...projectBranchGraph(snapshots, refs, undefined, 'head', 'main'),
  events: new Map(events.map(event => [event.id, event])),
  edgeBranches: new Map([['merge', new Map([['base', 'main'], ['tip', 'feature']])]]),
}, new Set(snapshots.map(node => node.id)));
const before = JSON.stringify(snapshots);
const compact = summarizePRGraph(projection, new Set());
assert.deepEqual(compact.snapshots.map(node => node.id), ['head', 'merge', 'birth', 'root']);
assert.deepEqual(compact.snapshots.find(node => node.id === 'merge')?.parents, ['root', 'birth']);
assert.deepEqual([...compact.edgeBranches.get('merge')!], [['root', 'main'], ['birth', 'feature']]);
assert.equal(compact.hiddenCount, 3);
assert.equal(compact.collapsedCounts.get('merge'), 3);
assert.equal(compact.events.get('merge')?.snapshot, 'tip', 'PR selection still opens the original source');
assert.equal(JSON.stringify(snapshots), before);
assert.deepEqual(projection.snapshots.find(node => node.id === 'merge')?.parents, ['base', 'tip']);
assert.deepEqual(new GraphIndex(compact.snapshots).issues, []);
assert.notEqual(layoutGraph(compact.snapshots, compact.pinHead).rows.find(row => row.snap.id === 'birth')?.lane, 0);

const selected = summarizePRGraph(projection, new Set(['middle']));
assert.ok(selected.snapshots.some(node => node.id === 'middle'));
assert.deepEqual(selected.snapshots.find(node => node.id === 'merge')?.parents, ['root', 'middle']);
assert.equal(selected.hiddenCount, 2);

const missing = visibleBranchGraph(projection, new Set(['head', 'base', 'tip', 'root']));
const folded = summarizePRGraph(missing, new Set());
assert.ok(!folded.snapshots.some(node => node.id === 'middle'));
assert.deepEqual(folded.snapshots.find(node => node.id === 'tip')?.parents, ['middle']);
assert.ok(folded.foldedParents.has('middle'), 'PR overview must not bridge archived or hidden captures');

const lifecycle = summarizePRGraph({ ...projection, lifecycleEdges: new Map([['merge', new Set(['tip'])]]) }, new Set());
assert.ok(lifecycle.snapshots.some(node => node.id === 'tip'));
assert.ok(lifecycle.lifecycleEdges.get('merge')?.has('tip'));
assert.ok(lifecycle.snapshots.find(node => node.id === 'merge')?.parents?.includes('tip'));

for (const data of [
  [snapshot('head', ['graft']), snapshot('graft', ['middle'], { grafted: true, graft_parents: ['side'] }), snapshot('middle', ['root']), snapshot('side', ['root']), snapshot('root')],
  [snapshot('head', ['missing'])],
  [snapshot('head', ['middle']), snapshot('middle', ['head'])],
  [snapshot('head'), snapshot('head')],
]) {
  const input = visibleBranchGraph(projectBranchGraph(data, refs, undefined, 'head', 'main'), new Set(data.map(node => node.id)));
  const result = summarizePRGraph(input, new Set());
  assert.deepEqual(new GraphIndex(result.snapshots).issues, new GraphIndex(input.snapshots).issues);
  if (data.some(node => node.grafted)) {
    assert.deepEqual(result.snapshots.find(node => node.id === 'graft')?.graft_parents, ['side']);
    assert.ok(result.snapshots.some(node => node.id === 'side'));
  }
  if (data.length === 1) assert.deepEqual(result.snapshots[0].parents, ['missing']);
}

const duplicate = summarizePRGraph({ ...projection, snapshots: [...projection.snapshots, projection.snapshots[0]] }, new Set());
assert.equal(duplicate.snapshots.length, projection.snapshots.length + 1, 'invalid structures are not concealed');
const pending = summarizePRGraph(projection, new Set(['middle', 'tip']));
assert.ok(pending.snapshots.some(node => node.id === 'tip'));
assert.ok(pending.snapshots.some(node => node.id === 'middle'));
const branched = summarizePRGraph({ ...projection, refs: [...projection.refs, { ...refs[0], name: 'feature', target: 'tip' }] }, new Set());
assert.ok(branched.snapshots.some(node => node.id === 'tip'), 'unmerged branch tips remain available');
const legacy = summarizePRGraph({ ...projection, snapshots: projection.snapshots.map(node => node.id === 'tip' ? { ...node, grafted: true } : node) }, new Set());
assert.deepEqual(legacy.snapshots.find(node => node.id === 'tip')?.parents, ['middle']);
assert.ok(legacy.snapshots.some(node => node.id === 'middle'), 'legacy graft targets keep their exact boundary');

const large = Array.from({ length: 100_000 }, (_, index) => snapshot(String(index), index ? [String(index - 1)] : []));
const largeProjection = visibleBranchGraph(projectBranchGraph(large, [], undefined, '99999', 'main'), new Set(large.map(node => node.id)));
const largeCompact = summarizePRGraph(largeProjection, new Set());
assert.equal(largeCompact.snapshots.length, 2);
assert.equal(largeCompact.hiddenCount, 99_998);
assert.deepEqual(largeCompact.snapshots[1].parents, ['0']);
assert.equal(largeCompact.collapsedCounts.get('99999'), 99_998);
assert.equal(largeCompact.events.size, 0, 'compacting commits cannot manufacture a PR');
assert.equal(summarizePRGraph({ ...projection, snapshots: [], events: new Map() }, new Set()).hiddenCount, 0);

for (const graft of [false, true]) {
  const diamond = [
    snapshot('merge', graft ? ['left'] : ['left', 'right'], graft ? { grafted: true, graft_parents: ['root'] } : {}),
    snapshot('left', ['left-middle']), snapshot('left-middle', ['root']),
    ...(!graft ? [snapshot('right', ['right-middle'], { branch: 'feature' }), snapshot('right-middle', ['root'], { branch: 'feature' })] : []),
    snapshot('root'),
  ];
  const input = visibleBranchGraph({
    ...projectBranchGraph(diamond, [], undefined, 'merge', 'main'),
    edgeBranches: new Map([['merge', new Map([['left', 'main'], ['right', 'feature']])]]),
  }, new Set(diamond.map(node => node.id)));
  const result = summarizePRGraph(input, new Set());
  const merge = result.snapshots.find(node => node.id === 'merge')!;
  assert.deepEqual(merge.parents, graft ? ['left'] : ['left', 'right'], 'converging arms must not collapse to duplicate parents');
  assert.deepEqual(result.snapshots.find(node => node.id === 'left')?.parents, ['root']);
  if (!graft) assert.deepEqual(result.snapshots.find(node => node.id === 'right')?.parents, ['root']);
  assert.equal(result.edgeBranches.get('merge')?.get('left'), 'main');
  if (!graft) assert.equal(result.edgeBranches.get('merge')?.get('right'), 'feature');
  assert.equal(layoutGraph(result.snapshots, result.pinHead).rows.find(row => row.snap.id === 'merge')?.branchesOut.length, 1);
  assert.equal(result.hiddenCount, graft ? 1 : 2);
}
