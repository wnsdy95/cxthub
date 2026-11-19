import assert from 'node:assert/strict';
import { anchoredPath, parseRoute, type AnchoredSelection } from '../src/route';

const repository = { id: 'repository-1', owner_username: 'alice', slug: 'cxthub' };
const sha = 'a'.repeat(40);
const selections: [AnchoredSelection | undefined, string][] = [
  [undefined, '/alice/cxthub?tab=sessions'],
  [{ kind: 'all' }, '/alice/cxthub?tab=sessions&show=all'],
  [{ kind: 'commit', sha }, `/alice/cxthub?tab=sessions&commit=${sha}`],
  [{ kind: 'branch', name: 'feature/login page' }, '/alice/cxthub?tab=sessions&branch=feature%2Flogin+page'],
  [{ kind: 'worktree', name: 'app' }, '/alice/cxthub?tab=sessions&worktree=app'],
  [{ kind: 'pull', number: 571 }, '/alice/cxthub?tab=sessions&pull=571'],
  [{ kind: 'worktree', name: 'app', machine: 'work laptop' }, '/alice/cxthub?tab=sessions&worktree=app&machine=work+laptop'],
  [{ kind: 'session', provider: 'claude', id: '0f4b3c2a-1d5e' }, '/alice/cxthub?tab=sessions&session=claude%3A0f4b3c2a-1d5e'],
  [{ kind: 'session', provider: 'codex', id: 'thread_1', segment: 3 }, '/alice/cxthub?tab=sessions&session=codex%3Athread_1&segment=3'],
  [{ kind: 'session', provider: 'codex', id: 'thread_1', rest: true }, '/alice/cxthub?tab=sessions&session=codex%3Athread_1&segment=rest'],
];
for (const [selection, path] of selections) {
  assert.equal(anchoredPath(repository, selection), path);
  const [pathname, search] = path.split('?');
  assert.deepEqual(parseRoute(pathname, `?${search}`), { kind: 'repository', username: 'alice', slug: 'cxthub', tab: 'sessions', ...(selection ? { anchored: selection } : {}) },
    `${path} reads back as the selection it was built from`);
}
assert.equal(anchoredPath({ id: 'repository-1', owner_username: '', slug: '' }, { kind: 'all' }), '/w/repository-1?tab=sessions&show=all');
assert.deepEqual(parseRoute('/w/repository-1', `?tab=sessions&commit=${sha}`), { kind: 'repositoryId', id: 'repository-1', tab: 'sessions', anchored: { kind: 'commit', sha } });

// A value that is not well formed selects nothing; it never reaches a request.
for (const search of [
  '?tab=sessions&commit=abc', `?tab=sessions&commit=${sha.toUpperCase()}`, '?tab=sessions&session=claude', '?tab=sessions&session=other:abc',
  '?tab=sessions&session=claude:a/b', `?tab=sessions&session=claude:${'x'.repeat(129)}`, '?tab=sessions&branch=', '?tab=sessions&worktree=a%0Ab', '?tab=sessions&show=everything', '?tab=sessions&pull=0', '?tab=sessions&pull=1e3',
]) {
  assert.deepEqual(parseRoute('/alice/cxthub', search), { kind: 'repository', username: 'alice', slug: 'cxthub', tab: 'sessions' }, search);
}
assert.deepEqual(parseRoute('/alice/cxthub', '?tab=sessions&session=claude:abc&segment=0'),
  { kind: 'repository', username: 'alice', slug: 'cxthub', tab: 'sessions', anchored: { kind: 'session', provider: 'claude', id: 'abc' } }, 'segments are numbered from one');
assert.deepEqual(parseRoute('/alice/cxthub', `?tab=onhold&commit=${sha}`), { kind: 'repository', username: 'alice', slug: 'cxthub', tab: 'onhold' }, 'only the sessions tab holds a selection');
assert.deepEqual(parseRoute('/alice/cxthub', `?commit=${sha}`), { kind: 'repository', username: 'alice', slug: 'cxthub' });
console.log('sessions tab routes: one selection in the address, read back exactly, malformed values dropped');
