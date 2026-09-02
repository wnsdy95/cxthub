import assert from 'node:assert/strict';
import { classifyGraphSnapshots } from '../src/graphStatus.ts';
import type { Ref, Snapshot } from '../src/types.ts';

const id = (ch: string) => `sha256:${ch.repeat(64)}`;
const root = id('1');
const pushed = id('2');
const archived = id('3');
const unpushed = id('4');
const uncommitted = id('5');
const snapshots: Snapshot[] = [
  { id: uncommitted, doc_hash: uncommitted, repo_id: 'repo', branch: 'main', parents: [unpushed], provider: 'codex', fidelity: 'full', message: 'hook: live', created_at: '2026-08-30T05:00:00Z' },
  { id: unpushed, doc_hash: unpushed, repo_id: 'repo', branch: 'main', parents: [pushed], provider: 'codex', fidelity: 'full', message: 'local', created_at: '2026-08-30T04:00:00Z' },
  { id: archived, doc_hash: archived, repo_id: 'repo', branch: 'feature/old', parents: [root], provider: 'claude', fidelity: 'full', message: 'old', created_at: '2026-08-30T03:00:00Z' },
  { id: pushed, doc_hash: pushed, repo_id: 'repo', branch: 'main', parents: [root], provider: 'codex', fidelity: 'full', message: 'shared', created_at: '2026-08-30T02:00:00Z' },
  { id: root, doc_hash: root, repo_id: 'repo', branch: 'main', parents: [], provider: 'codex', fidelity: 'full', message: 'root', created_at: '2026-08-30T01:00:00Z' },
];
const refs: Ref[] = [
  { kind: 'branch', name: 'main', repo_id: 'repo', target: pushed },
  {
    kind: 'tag',
    name: `cxt/branch-state/v1/00000000000000000001/archived/${'3'.repeat(64)}/feature/old`,
    repo_id: 'repo',
    target: archived,
  },
];

const status = classifyGraphSnapshots(refs, snapshots, new Set([uncommitted]));
assert.deepEqual([...status.pushed].sort(), [pushed, root].sort());
assert.deepEqual([...status.unpushed], [unpushed]);
assert.deepEqual([...status.uncommitted], [uncommitted]);
assert.deepEqual([...status.archivedOnly], [archived]);
assert.equal(status.archivedBranches, 1);
assert.deepEqual(status.archived, [
  { branch: 'feature/old', target: archived, uniqueCount: 1, targetAvailable: true },
]);
assert.equal(
  status.pushed.size + status.unpushed.size + status.uncommitted.size + status.archivedOnly.size,
  snapshots.length,
  'every graph snapshot must have exactly one visible workflow/lifecycle tier',
);

// Overlay grafts are first-class reachability edges. An archived tip appended
// into the active branch remains visible/pushed, while a local child above that
// shared graft is still unpushed.
const graftedHead = id('6');
const localAboveGraft = id('7');
const graftSnapshots: Snapshot[] = [
  {
    id: localAboveGraft,
    doc_hash: localAboveGraft,
    repo_id: 'repo',
    branch: 'main',
    parents: [graftedHead],
    provider: 'codex',
    fidelity: 'full',
    message: 'local after append',
    created_at: '2026-08-30T07:00:00Z',
  },
  {
    id: graftedHead,
    doc_hash: graftedHead,
    repo_id: 'repo',
    branch: 'main',
    parents: [pushed],
    graft_parents: [archived],
    grafted: true,
    provider: 'codex',
    fidelity: 'full',
    message: 'appended branch context',
    created_at: '2026-08-30T06:00:00Z',
  },
  ...snapshots.filter((snapshot) => snapshot.id !== unpushed && snapshot.id !== uncommitted),
];
const graftRefs: Ref[] = [
  { kind: 'branch', name: 'main', repo_id: 'repo', target: graftedHead },
  refs[1],
];
const graftStatus = classifyGraphSnapshots(graftRefs, graftSnapshots);
assert.equal(graftStatus.pushed.has(archived), true, 'graft-reachable archived history stays visible');
assert.equal(graftStatus.archivedOnly.has(archived), false, 'active graft history must not collapse');
assert.equal(graftStatus.archived[0]?.uniqueCount, 0, 'fully shared archived history remains discoverable');
assert.deepEqual([...graftStatus.unpushed], [localAboveGraft]);
