import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api } from '../src/api';

test('session reports are read from the repository under their escaped identifiers', async () => {
  const originalFetch = globalThis.fetch;
  const requests: [string, RequestInit][] = [];
  globalThis.fetch = async (url, options) => {
    requests.push([String(url), options!]);
    return new Response('[]');
  };
  try {
    const repo = `sha256:${'a'.repeat(64)}`, sha = 'b'.repeat(40);
    await api.anchoredSessions(repo);
    await api.anchoredSession(repo, 'claude', '0f4b3c2a.session_1');
    await api.anchoredCommit(repo, sha);
    await api.branchLifetimes(repo);
    const base = `/api/v1/repos/sha256%3A${'a'.repeat(64)}`;
    assert.deepEqual(requests.map(([url]) => url), [
      `${base}/anchored-sessions`, `${base}/anchored-sessions/claude/0f4b3c2a.session_1`, `${base}/anchored-commits/${sha}`, `${base}/branch-lifetimes`,
    ]);
    assert.equal(requests.every(([, options]) => options.method === 'GET' && options.credentials === 'include'), true);
    assert.equal(requests.some(([, options]) => new Headers(options.headers).has('X-Cxt-CSRF')), false, 'reads carry no write marker');
  } finally { globalThis.fetch = originalFetch; }
});
