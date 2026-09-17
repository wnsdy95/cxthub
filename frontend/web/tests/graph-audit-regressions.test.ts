import assert from 'node:assert/strict';
import { projectBranchGraph } from '../src/graphProjection.ts';
import { layoutGraph } from '../src/graph.ts';
import { classifyBranchHistoryMarkers, classifyGraphSnapshots } from '../src/graphStatus.ts';
import { historicalSnapshotIds } from '../src/contextHistory.ts';
import { repositoryGraph } from '../src/repositoryGraph.ts';
import { sharedReachable } from '../src/onhold.ts';
import type { HistoryEvent, Pending, Ref, Snapshot, Unsync } from '../src/types.ts';

const at = (n: number) => new Date(Date.UTC(2026, 8, 18, 0, 0, n)).toISOString();
const snap = (id: string, branch: string, parents: string[], n: number, message = id): Snapshot =>
  ({ id, repo_id: 'repo', doc_hash: id, branch, parents, created_at: at(n), message, provider: 'codex', fidelity: 'full' });
const ref = (name: string, target: string, branch_id?: string): Ref => ({ kind: 'branch', repo_id: 'repo', name, target, branch_id });
const birth: HistoryEvent = { id: 'birth', repo_id: 'repo', branch_id: 'feature-id', kind: 'birth', branch: 'feature', source: 'root', target: 'root', created_at: at(1) };
const mainBirth: HistoryEvent = { ...birth, id: 'main-birth', branch_id: 'main-id', branch: 'main', created_at: at(0) };
const done: HistoryEvent = { ...mainBirth, id: 'done', kind: 'pr-merge', source: 'feature', target: 'feature', shared_target: 'root', source_branch_id: birth.branch_id,
  pr_completed: true, pr: { number: 1, head_branch: 'feature', base_branch: 'main', head_sha: 'a'.repeat(40), merge_sha: 'b'.repeat(40) }, created_at: at(5) };
const source = [snap('root', 'main', [], 0), snap('feature', 'feature', ['root'], 3)];
const rename: HistoryEvent = { ...mainBirth, id: 'rename', kind: 'rename', branch: 'trunk', previous_branch: 'main', binding_parent: mainBirth.id, created_at: at(6) };
const merged = 'graph:merge:done';
for (const hs of [[mainBirth, birth, done, rename], [rename, done, birth, mainBirth]]) {
  const p = projectBranchGraph(source, [ref('trunk', 'feature', 'main-id')], hs, [], 'feature', 'trunk');
  assert.equal(p.pinHead, merged, 'rename preserves the same merge placement');
  assert.equal(p.refs[0].target, merged);
  assert.equal(p.events.get(merged)?.branch, 'trunk');
  assert.equal(layoutGraph(p.snapshots, p.pinHead).rows.find(r => r.snap.id === merged)?.lane, 0);
  const reused: HistoryEvent = { ...mainBirth, id: 'reuse', branch_id: 'new-main', binding_parent: rename.id, created_at: at(7) };
  const q = projectBranchGraph(source, [ref('trunk', 'feature', 'main-id'), ref('main', 'feature', 'new-main')], [...hs, reused], [], 'feature', 'main');
  assert.equal(q.pinHead, 'feature', 'reused name cannot take the old identity merge');
  assert.equal(q.refs.find(r => r.name === 'main')?.target, 'feature');
}

const current = snap('current', 'main', ['feature'], 2); // provider clock before completion
const p = projectBranchGraph([...source, current], [ref('main', 'current', 'main-id')], [mainBirth, birth, done], [], 'current', 'main');
assert.deepEqual(p.snapshots.find(s => s.id === 'current')?.parents, [merged]);
assert.equal(layoutGraph(p.snapshots, p.pinHead).rows.find(r => r.snap.id === merged)?.lane, 0);
const rewind: HistoryEvent = { ...mainBirth, id: 'rewind', kind: 'advance', source: 'current', target: 'feature', created_at: at(7) };
const rewound = projectBranchGraph([...source, current], [ref('main', 'feature', 'main-id')], [mainBirth, birth, done, rewind], [], 'feature', 'main');
assert.equal(rewound.pinHead, 'feature', 'rewind must not reactivate an old completed placement');
assert.deepEqual(rewound.snapshots.find(s => s.id === 'current')?.parents, ['feature']);

const pending: Pending = { repo_id: 'repo', session_id: 'live', branch: 'main', provider: 'codex', target: 'C', updated_at: at(3) };
const chain = [snap('A', 'main', [], 0), snap('B', 'main', ['A'], 1, 'hook: older'), snap('C', 'main', ['B'], 2, 'hook: latest')];
const refs = [ref('main', 'A')];
const shared = sharedReachable(refs, chain);
for (const kind of ['position', 'attach', 'birth', 'orphan'] as const) {
  const event: HistoryEvent = { ...birth, kind, source: 'C', target: 'C', memory_source: 'B' };
  assert.deepEqual([...historicalSnapshotIds([], chain, [event])], [], `${kind} does not publish anything`);
  const graph = repositoryGraph(chain, refs, [event], shared, [pending], []);
  const status = classifyGraphSnapshots(refs, graph.graphSnapshots, graph.uncommittedIds, 'main', historicalSnapshotIds([], chain, [event]));
  assert.deepEqual([...status.uncommitted].sort(), ['B', 'C']);
  assert.deepEqual([...status.pushed], ['A']);
}
const unsync = { repo_id: 'repo', branch: 'main', target: 'C', user: 'alice', updated_at: at(3) } as Unsync;
const unsynced = repositoryGraph(chain, refs, [], shared, [pending], [unsync]);
assert.equal(unsynced.graphSnapshots.length, 3, 'unsync hook captures and their parents stay visible');
assert.equal(unsynced.uncommittedIds.size, 0, 'committed unsync path supersedes pending');
assert.deepEqual([...classifyGraphSnapshots(refs, chain, unsynced.uncommittedIds).unpushed].sort(), ['B', 'C']);
const published = historicalSnapshotIds([], chain, [{ ...birth, kind: 'publish', target: 'C' }]);
const committed = repositoryGraph(chain, refs, [], published, [pending], [unsync]);
assert.equal(committed.uncommittedIds.size, 0);
assert.deepEqual([...classifyGraphSnapshots(refs, chain, committed.uncommittedIds, 'main', published).pushed].sort(), ['A', 'B', 'C']);
const dismissed = repositoryGraph(chain, refs, [], shared, [{ ...pending, dismissed: true }], []);
assert.deepEqual(dismissed.graphSnapshots.map(s => s.id), ['A']);

const hash = `sha256:${'1'.repeat(64)}`;
const archive: Ref = { kind: 'tag', repo_id: 'repo', name: `cxt/branch-state/v1/00000000000000000001/archived/${'1'.repeat(64)}/unused`, target: hash };
assert.equal(classifyBranchHistoryMarkers([ref('main', hash), archive], [snap(hash, 'main', [], 0)])[0].kind, 'archived', 'unused branch deletion is not a join');
const tag: Ref = { kind: 'tag', repo_id: 'repo', name: 'v1', target: 'C' };
const tagged = classifyGraphSnapshots([tag], chain);
assert.equal(tagged.unpushed.size, 0);
assert.equal(tagged.pushed.size, 0, 'tag is not branch publication');
assert.deepEqual([...tagged.tagged].sort(), ['A', 'B', 'C']);
const taggedPending = classifyGraphSnapshots([tag], chain, new Set(['C']));
assert.ok(taggedPending.uncommitted.has('C'), 'tag cannot commit a pending capture');
assert.ok(!taggedPending.tagged.has('C'));
const incomplete = layoutGraph([snap('child', 'main', ['missing'], 0)], 'child');
assert.deepEqual(incomplete.rows[0].outgoing, [], 'missing parents cannot leave a dangling lane');
