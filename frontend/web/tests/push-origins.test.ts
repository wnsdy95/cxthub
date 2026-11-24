import assert from 'node:assert/strict';
import { test } from 'node:test';
import { commitOrigin, ownFailingCollectors, pushOriginsByHead } from '../src/pushOrigins';
import { buildAnchoredIndex } from '../src/anchoredSessions';
import type { PushOriginView } from '../src/types';

const sha = (digit: string) => digit.repeat(40);
const at = '2026-11-24T10:00:00Z';

const view: PushOriginView = {
  collectors: [
    { by: 'alice', machine: 'laptop', heard_at: at, failing: true, run_error: 'the upload was refused', own: true },
    { by: 'bob', machine: 'desk', heard_at: at, failing: false, own: false },
    { by: 'alice', machine: 'old', heard_at: at, failing: true, own: true },
  ],
  pushes: [
    { branch: 'topic', sha: sha('d'), at, state: 'unknown', waiting: [1, 0, 9] },
    { branch: 'main', sha: sha('c'), at, state: 'no_cxt' },
    { branch: 'main', sha: sha('b'), at, state: 'cxt_error', collector: 0, claimed_at: at },
    // The same commit on two branches: first seen unclaimed, then claimed.
    { branch: 'release', sha: sha('a'), at, state: 'no_cxt' },
    { branch: 'main', sha: sha('a'), at, state: 'cxt', collector: 1, claimed_at: at },
    // Claimed first in the list, unclaimed later: the claim stays.
    { branch: 'main', sha: sha('e'), at, state: 'cxt', collector: 1, claimed_at: at },
    { branch: 'copy', sha: sha('e'), at, state: 'unknown', waiting: [0] },
    // Two pushes nobody claimed: the one listed first, the newer, is kept.
    { branch: 'one', sha: sha('f'), at, state: 'unknown', waiting: [0] },
    { branch: 'two', sha: sha('f'), at, state: 'no_cxt' },
  ],
};

test('each push is found by its newest commit, with the collectors it names', () => {
  const byHead = pushOriginsByHead(view);
  assert.equal(byHead.size, 6);
  const unknown = byHead.get(sha('d'));
  assert.equal(unknown?.push.state, 'unknown');
  assert.equal(unknown?.collector, undefined);
  // The collectors that are waited for, in the order the server named them; a position the list does not hold is left out.
  assert.deepEqual(unknown?.waiting.map(collector => collector.machine), ['desk', 'laptop']);
  const failed = byHead.get(sha('b'));
  assert.equal(failed?.push.state, 'cxt_error');
  assert.equal(failed?.collector?.machine, 'laptop');
  assert.deepEqual(failed?.waiting, []);
  assert.equal(byHead.get(sha('c'))?.push.state, 'no_cxt');
  // A push a collector claimed says more about a commit than one nobody claimed, whichever comes first.
  assert.equal(byHead.get(sha('a'))?.push.branch, 'main');
  assert.equal(byHead.get(sha('a'))?.collector?.by, 'bob');
  assert.equal(byHead.get(sha('e'))?.push.branch, 'main');
  assert.equal(byHead.get(sha('f'))?.push.branch, 'one');
  // No answer from the server: nothing is known, and nothing breaks.
  assert.equal(pushOriginsByHead(undefined).size, 0);
});

test('a commit came from where the push it came with came from', () => {
  const byHead = pushOriginsByHead(view);
  assert.equal(commitOrigin(byHead, sha('b'))?.push.state, 'cxt_error');
  // A commit that came with no push the server saw, or with one it no longer lists, has no origin.
  assert.equal(commitOrigin(byHead, undefined), undefined);
  assert.equal(commitOrigin(byHead, ''), undefined);
  assert.equal(commitOrigin(byHead, sha('9')), undefined);
});

test('the commit rows of the tab carry the push each commit came with', () => {
  const commit = (letter: string, head?: string) => ({ sha: sha(letter), parents: [], authored_at: at, committed_at: at, subject: letter, ...(head ? { head } : {}) });
  const index = buildAnchoredIndex([], [], { remote: { read: true, defaultBranch: 'main', branches: [], pulls: [], commits: [commit('1', sha('2')), commit('2', sha('2')), commit('3')] } });
  assert.equal(index.commitBySha.get(sha('1'))?.pushHead, sha('2'));
  assert.equal(index.commitBySha.get(sha('2'))?.pushHead, sha('2'));
  assert.equal(index.commitBySha.get(sha('3'))?.pushHead, undefined);
});

test('the owner of a failing collector is told, and nobody else', () => {
  assert.deepEqual(ownFailingCollectors(view).map(collector => collector.machine), ['laptop', 'old']);
  const others: PushOriginView = { collectors: [{ by: 'alice', machine: 'laptop', heard_at: at, failing: true, own: false }, { machine: 'mine', heard_at: at, failing: false, own: true }], pushes: [] };
  assert.deepEqual(ownFailingCollectors(others), []);
  assert.deepEqual(ownFailingCollectors(undefined), []);
});
