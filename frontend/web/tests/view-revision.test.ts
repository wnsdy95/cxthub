import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api } from '../src/api';
import { followRevision, SESSIONS_FALLBACK_MS, SESSIONS_TAB_QUERIES, VIEW_REVISION_MS } from '../src/viewRevision';

test('the mark of a repository is read with one small request', async () => {
  const originalFetch = globalThis.fetch;
  const requests: [string, RequestInit][] = [];
  globalThis.fetch = async (url, options) => {
    requests.push([String(url), options!]);
    return new Response('{"revision":"12.0.3"}');
  };
  try {
    const repo = `sha256:${'a'.repeat(64)}`;
    assert.deepEqual(await api.viewRevision(repo), { revision: '12.0.3' });
    assert.deepEqual(requests.map(([url]) => url), [`/api/v1/repos/sha256%3A${'a'.repeat(64)}/revision`]);
    assert.equal(requests[0][1].method, 'GET');
    assert.equal(requests[0][1].body, undefined);
    assert.equal(new Headers(requests[0][1].headers).has('X-Cxt-CSRF'), false, 'a read carries no write marker');
  } finally { globalThis.fetch = originalFetch; }
});

test('the lists are read again when the mark is another one, and only then', () => {
  const all = [...SESSIONS_TAB_QUERIES], none: string[] = [];
  // The first mark of a repository is remembered: the lists were just read.
  const opened = followRevision(null, 'repo-a', '5.0.1');
  assert.deepEqual(opened, { seen: { repo: 'repo-a', revision: '5.0.1' }, readAgain: none });
  // The same mark again: nothing changed, nothing is read.
  const same = followRevision(opened.seen, 'repo-a', '5.0.1');
  assert.deepEqual(same, { seen: { repo: 'repo-a', revision: '5.0.1' }, readAgain: none });
  // Another mark: everything the tab reads is read again, and the new mark is remembered.
  const moved = followRevision(same.seen, 'repo-a', '6.0.1');
  assert.deepEqual(moved, { seen: { repo: 'repo-a', revision: '6.0.1' }, readAgain: all });
  assert.deepEqual(followRevision(moved.seen, 'repo-a', '6.0.1').readAgain, none);
  // Marks have no order: one that looks smaller is still another one.
  assert.deepEqual(followRevision(moved.seen, 'repo-a', '5.0.1').readAgain, all);
  // The mark of another repository says nothing about this one's lists: the
  // page opened that repository and read its lists then.
  const other = followRevision(moved.seen, 'repo-b', '6.0.1');
  assert.deepEqual(other, { seen: { repo: 'repo-b', revision: '6.0.1' }, readAgain: none });
  assert.deepEqual(followRevision(other.seen, 'repo-b', '7.0.1').readAgain, all);
  assert.deepEqual(followRevision(moved.seen, 'repo-b', '9.9.9').readAgain, none);
});

test('an open tab asks every few seconds and falls back to minutes', () => {
  assert.equal(VIEW_REVISION_MS, 3_000);
  assert.equal(SESSIONS_FALLBACK_MS, 300_000);
  // Everything the tab reads for a repository is read again together.
  for (const key of ['anchored-sessions', 'anchored-commit', 'anchored-commit-summaries', 'anchored-notes', 'branch-lifetimes', 'reported-worktrees', 'remote-git', 'push-origins']) {
    assert.equal((SESSIONS_TAB_QUERIES as readonly string[]).includes(key), true, key);
  }
  assert.equal(new Set(SESSIONS_TAB_QUERIES).size, SESSIONS_TAB_QUERIES.length);
});
