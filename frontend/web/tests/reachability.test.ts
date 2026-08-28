import assert from 'node:assert/strict';
import { mainlineOf } from '../src/graph.ts';
import { reachableSnapshotIds, sharedReachable } from '../src/onhold.ts';
import type { Ref, Snapshot } from '../src/types.ts';

function snapshot(id: string, parents: string[] = [], graftParents: string[] = []): Snapshot {
  return {
    id,
    repo_id: 'repo',
    branch: 'main',
    parents,
    graft_parents: graftParents,
    doc_hash: id,
    provider: 'codex',
    fidelity: 'full',
    message: id,
    created_at: '2026-08-28T00:00:00Z',
  };
}

const snapshots = [
  snapshot('unreachable'),
  snapshot('root'),
  snapshot('graft-root', [], ['merge']), // cycle must terminate
  snapshot('natural', ['root']),
  snapshot('merge', ['natural'], ['graft-root', 'graft-root']),
  snapshot('head', ['merge']),
];

assert.deepEqual(
  [...reachableSnapshotIds(['head'], snapshots)].sort(),
  ['graft-root', 'head', 'merge', 'natural', 'root'],
  'branch reachability must include natural and graft parents exactly once',
);
assert.deepEqual(
  [...reachableSnapshotIds(['missing'], snapshots)],
  [],
  'missing targets must not manufacture reachable snapshots',
);
assert.deepEqual(
  [...mainlineOf('head', snapshots)],
  ['head', 'merge', 'natural', 'root'],
  'first-parent styling must continue to exclude graft side histories',
);

const refs: Ref[] = [
  { kind: 'branch', name: 'main', repo_id: 'repo', target: 'head' },
  { kind: 'session', name: 'remaining', repo_id: 'repo', target: 'graft-root' },
  { kind: 'tag', name: 'unshared', repo_id: 'repo', target: 'unreachable' },
];
assert.equal(sharedReachable(refs, snapshots).has('unreachable'), false, 'tags are not shared timeline roots');
