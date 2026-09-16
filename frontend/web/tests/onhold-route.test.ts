import assert from 'node:assert/strict';
import { findRepositoryByRoute, parseRoute, repoPath, resolvedWorkspaceTab, wsPath } from '../src/route';
import type { Repo } from '../src/types';

const workspace = { id: 'ws-1', owner_username: 'alice', slug: 'cxthub' };
const legacy = { id: 'legacy', remote_url: 'https://cxthub.com/alice/cxthub' } as Repo;
const named = { id: 'named', remote_url: 'https://cxthub.com/alice/cxthub/onhold' } as Repo;
const repos = [named, legacy];
assert.equal(findRepositoryByRoute(parseRoute('/alice/cxthub', ''), repos)?.id, legacy.id);

assert.equal(repoPath(workspace, legacy, 'onhold'), '/alice/cxthub?tab=onhold');
assert.equal(wsPath(workspace, 'onhold'), '/alice/cxthub?tab=onhold');
const legacyView = parseRoute('/alice/cxthub', '?tab=onhold');
assert.equal(resolvedWorkspaceTab(legacyView, repos), 'onhold');
assert.equal(findRepositoryByRoute(legacyView, repos)?.id, legacy.id);

// Retired separator routes cannot select a view, even with a tab query.
const oldView = parseRoute('/alice/cxthub/-/onhold', '');
assert.deepEqual(oldView, { kind: 'notFound' });
assert.equal(findRepositoryByRoute(oldView, repos), undefined);

assert.equal(repoPath(workspace, named), '/alice/cxthub/onhold');
const namedContext = parseRoute('/alice/cxthub/onhold', '');
assert.equal(findRepositoryByRoute(namedContext, repos)?.id, named.id);
assert.equal(resolvedWorkspaceTab(namedContext, repos), undefined);
assert.equal(repoPath(workspace, named, 'onhold'), '/alice/cxthub/onhold?tab=onhold');
const namedView = parseRoute('/alice/cxthub/onhold', '?tab=onhold');
assert.equal(findRepositoryByRoute(namedView, repos)?.id, named.id);
assert.equal(resolvedWorkspaceTab(namedView, repos), 'onhold');

const idView = parseRoute('/w/ws-1', '?tab=onhold');
assert.equal(resolvedWorkspaceTab(idView, repos), 'onhold');
assert.equal(findRepositoryByRoute(idView, repos)?.id, legacy.id);
assert.equal(resolvedWorkspaceTab(parseRoute('/alice/cxthub', '?tab=invalid'), repos), undefined);
assert.deepEqual(parseRoute('/alice/cxthub/-/invalid', ''), { kind: 'notFound' });

for (const tab of ['members', 'connections', 'settings', 'onhold'] as const) {
  assert.equal(wsPath(workspace, tab), `/alice/cxthub?tab=${tab}`);
  assert.equal(resolvedWorkspaceTab(parseRoute('/alice/cxthub', `?tab=${tab}`), repos), tab);
  assert.equal(resolvedWorkspaceTab(parseRoute('/w/ws-1', `?tab=${tab}`), repos), tab);
  assert.deepEqual(parseRoute(`/alice/cxthub/-/${tab}`, `?tab=${tab}`), { kind: 'notFound' });
  assert.deepEqual(parseRoute(`/w/ws-1/-/${tab}`, ''), { kind: 'notFound' });
  const sameName = { id: tab, remote_url: `https://cxthub.com/alice/cxthub/${tab}` } as Repo;
  const sameNameRoute = parseRoute(`/alice/cxthub/${tab}`, '');
  assert.equal(resolvedWorkspaceTab(sameNameRoute, [sameName]), undefined);
  assert.equal(findRepositoryByRoute(sameNameRoute, [sameName])?.id, tab);
}
assert.deepEqual(parseRoute('/alice/cxthub/%2D/settings', '?tab=members'), { kind: 'notFound' });

console.log('tab routes: query tabs and repository names remain distinct; separator routes are retired');
