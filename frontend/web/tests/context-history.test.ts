import assert from 'node:assert/strict';
import { previousProgressGroups, hiddenProgressIds, historicalSnapshotIds } from '../src/contextHistory.ts';
import { classifyGraphSnapshots } from '../src/graphStatus.ts';
import { holdCounts, orphanPendings, unsyncChains } from '../src/onhold.ts';
import type { HistoryEvent, Ref, RefLogEntry, Snapshot } from '../src/types.ts';

const snapshot = (id: string, parents: string[] = [], graft_parents: string[] = []): Snapshot => ({
  id, doc_hash: id, repo_id: 'repo', branch: 'main', parents, graft_parents,
  provider: 'codex', fidelity: 'full', message: id, created_at: '2026-09-15T00:00:00Z',
});
const snapshots = [snapshot('A'), snapshot('B', ['A']), snapshot('C', ['B']), snapshot('D', ['A'])];
const refs: Ref[] = [{ kind: 'branch', name: 'main', target: 'D', repo_id: 'repo' }];
const move = (old: string, next: string, name = 'main'): RefLogEntry => ({
  kind: 'branch', name, old, new: next, created_at: '2026-09-15T01:00:00Z',
});
const logs = [move('A', 'D'), move('C', 'A'), move('B', 'C'), move('A', 'B')];
const original = JSON.stringify({ refs, snapshots, logs });
const groups = previousProgressGroups(refs, snapshots, logs);
assert.equal(groups.length, 1);
assert.equal(groups[0].before, 'C');
assert.equal(groups[0].after, 'A');
assert.deepEqual([...groups[0].snapshotIds].sort(), ['B', 'C']);
assert.deepEqual([...hiddenProgressIds(groups, new Set())].sort(), ['B', 'C']);
assert.equal(hiddenProgressIds(groups, new Set([groups[0].key])).size, 0);
assert.equal(JSON.stringify({ refs, snapshots, logs }), original, 'folding never changes graph data or ref evidence');

const recorded = historicalSnapshotIds(logs, snapshots);
assert.deepEqual([...recorded].sort(), ['A', 'B', 'C', 'D']);
const status = classifyGraphSnapshots(refs, snapshots, new Set(), 'main', recorded);
assert.equal(status.unpushed.size, 0, 'previously published context must not become unpushed after a rewind');
const pending = [{ repo_id: 'repo', session_id: 'old-session', branch: 'main', provider: 'codex', target: 'C', updated_at: '' }];
const unsync = [{ repo_id: 'repo', user: 'author', branch: 'main', target: 'C', updated_at: '' }];
const clusters = unsyncChains(unsync, snapshots, recorded);
assert.equal(clusters.length, 0, 'a stale push pointer does not reclassify published history');
assert.equal(orphanPendings(pending, refs, snapshots, clusters, recorded).length, 0);
assert.equal(holdCounts(refs, snapshots, unsync, pending, recorded).size, 0, 'tab badges use the same history evidence');

// A teammate's active branch, or a new local child, keeps its complete path.
const active = previousProgressGroups([...refs, { kind: 'branch', name: 'teammate', target: 'C', repo_id: 'repo' }], snapshots, logs);
assert.equal(hiddenProgressIds(active, new Set()).size, 0);
const local = previousProgressGroups(refs, [...snapshots, snapshot('LOCAL', ['C'])], logs);
assert.equal(hiddenProgressIds(local, new Set()).size, 0);

// Independent earlier paths fold separately. Their common retained ancestor
// stays visible when either path is expanded.
const multiple = previousProgressGroups(refs, [...snapshots, snapshot('E', ['B'])], [...logs, move('E', 'A')]);
assert.equal(multiple.length, 2);
const c = multiple.find((g) => g.before === 'C')!;
assert.deepEqual([...hiddenProgressIds(multiple, new Set([c.key]))], ['E']);

// A grafted parent is real ancestry, including when timestamps are out of order.
const grafted = [snapshot('A'), snapshot('B', ['A']), snapshot('C', ['B']), snapshot('D', [], ['C'])];
assert.deepEqual(previousProgressGroups(refs, grafted, logs), []);
assert.deepEqual(previousProgressGroups(refs, snapshots, [move('', 'A'), move('C', ''), move('C', 'C')]), []);
assert.deepEqual(previousProgressGroups(refs, snapshots, []), [], 'shared hashes without a recorded move never imply a rewind');
assert.deepEqual(previousProgressGroups(refs, snapshots, [move('unknown', 'A')]), []);
assert.deepEqual(previousProgressGroups(refs, [snapshot('D', ['missing']), ...snapshots.filter(s => s.id !== 'D')], logs), [], 'incomplete current ancestry cannot prove that a tip was abandoned');
assert.equal(previousProgressGroups(refs, snapshots, [...logs, ...logs]).length, 1, 'duplicate evidence does not duplicate groups');
assert.deepEqual(previousProgressGroups(refs, snapshots.filter(s => s.id !== 'B'), logs), [], 'incomplete old ancestry stays visible');
assert.deepEqual(previousProgressGroups(refs, snapshots, [move('C', 'D')]).map(g => [...g.snapshotIds].sort()), [['B', 'C']], 'recorded divergent moves also preserve earlier progress');

const event: HistoryEvent = { id: 'operation', repo_id: 'repo', branch_id: 'branch-main', branch: 'main', kind: 'advance', source: 'C', target: 'D', created_at: '2026-09-15T01:00:00Z' };
const serverEvents = previousProgressGroups(refs, snapshots, [], [event]);
assert.deepEqual([...hiddenProgressIds(serverEvents, new Set())].sort(), ['B', 'C'], 'durable history works without legacy reflog');
assert.deepEqual([...historicalSnapshotIds([], snapshots, [event])].sort(), ['A', 'B', 'C', 'D']);
const browsing = previousProgressGroups(refs, snapshots, [], [event], { branch: 'main', snapshot: 'A' });
assert.deepEqual([...hiddenProgressIds(browsing, new Set())].sort(), ['B', 'C', 'D'], 'explicit past view folds both later paths without changing refs');
assert.equal(refs[0].target, 'D');
const teammateBrowsing = previousProgressGroups([...refs, { kind: 'branch', name: 'teammate', target: 'C', repo_id: 'repo' }], snapshots, [], [event], { branch: 'main', snapshot: 'A' });
assert.deepEqual([...hiddenProgressIds(teammateBrowsing, new Set())], ['D']);

// Rename/reuse does not move an older continuation into a new same-name task.
const birth: HistoryEvent = { ...event, id: 'birth', kind: 'birth', source: 'A', target: 'A', branch: 'old' };
const advance = { ...event, id: 'advance', branch: 'old' };
const renamed: HistoryEvent = { ...birth, id: 'rename', kind: 'rename', branch: 'new', previous_branch: 'old', binding_parent: birth.id };
const reused: HistoryEvent = { ...birth, id: 'reuse', branch_id: 'another-task', binding_parent: renamed.id };
const renamedRefs: Ref[] = [{ ...refs[0], name: 'new' }, { ...refs[0], name: 'old', target: 'A' }];
const identityHistory = [reused, renamed, advance, birth];
const renamedGroups = previousProgressGroups(renamedRefs, snapshots, [], identityHistory);
assert.equal(renamedGroups.length, 1);
assert.equal(renamedGroups[0].branch, 'new');
assert.deepEqual([...hiddenProgressIds(renamedGroups, new Set())].sort(), ['B', 'C']);
const oldPosition = previousProgressGroups(renamedRefs, snapshots, [], identityHistory, { branch: 'old', branch_id: birth.branch_id, snapshot: 'A' });
assert.deepEqual([...hiddenProgressIds(oldPosition, new Set())].sort(), ['B', 'C', 'D']);
const reusedPosition = previousProgressGroups(renamedRefs, snapshots, [], identityHistory, { branch: 'old', branch_id: reused.branch_id, snapshot: 'A' });
assert.deepEqual([...hiddenProgressIds(reusedPosition, new Set())].sort(), ['B', 'C'], 'browsing the reused name cannot replace the original task position');
