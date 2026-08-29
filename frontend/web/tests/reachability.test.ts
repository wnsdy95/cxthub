import assert from 'node:assert/strict';
import { mainlineOf } from '../src/graph.ts';
import { reachableSnapshotIds, sharedReachable } from '../src/onhold.ts';
import { archivedBranchMarkers, parseBranchLifecycleRef, projectBranchRefs } from '../src/branchLifecycle.ts';
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

const archiveTarget = 'sha256:' + 'a'.repeat(64);
const archiveRef: Ref = {
  kind: 'tag',
  name: `cxt/branch-state/v1/00000000000000000001/archived/${'a'.repeat(64)}/feature/deleted`,
  repo_id: 'repo',
  target: archiveTarget,
};
const archivedRefs: Ref[] = [
  { kind: 'branch', name: 'feature/deleted', repo_id: 'repo', target: archiveTarget },
  archiveRef,
];
assert.equal(parseBranchLifecycleRef(archiveRef)?.branch, 'feature/deleted');
assert.equal(projectBranchRefs(archivedRefs).some((r) => r.kind === 'branch'), false, 'archived branch projection must be hidden');
assert.equal(
  sharedReachable(archivedRefs, [snapshot(archiveTarget)]).has(archiveTarget),
  true,
  'archived lifecycle tags must preserve shared reachability',
);

const activeRef: Ref = {
  kind: 'tag',
  name: `cxt/branch-state/v1/00000000000000000001/active/${'a'.repeat(64)}/feature/deleted`,
  repo_id: 'repo',
  target: archiveTarget,
};
assert.equal(
  projectBranchRefs([...archivedRefs, activeRef]).some((r) => r.kind === 'branch'),
  true,
  'same-generation active event must preserve the branch',
);

const advancedTarget = 'sha256:' + 'b'.repeat(64);
assert.equal(
  projectBranchRefs([
    { kind: 'branch', name: 'feature/deleted', repo_id: 'repo', target: advancedTarget },
    archiveRef,
  ]).some((r) => r.kind === 'branch' && r.target === advancedTarget),
  true,
  'an archive may hide only the exact target it observed',
);

const interruptedArchive = projectBranchRefs([
  { kind: 'head', name: 'HEAD', repo_id: 'repo', target: '', symbolic: 'feature/deleted' },
  ...archivedRefs,
]);
const projectedHead = interruptedArchive.find((ref) => ref.kind === 'head');
assert.equal(projectedHead?.symbolic, '', 'interrupted archive must not expose dangling symbolic HEAD');
assert.equal(projectedHead?.target, archiveTarget, 'interrupted archive HEAD detaches at the preserved target');
assert.deepEqual(
  archivedBranchMarkers(archivedRefs),
  [{ branch: 'feature/deleted', target: archiveTarget }],
  'archived branch must remain visible as a recoverable graph marker',
);
assert.deepEqual(
  archivedBranchMarkers([...archivedRefs, activeRef]),
  [],
  'an active winner must remove the archived graph marker',
);
