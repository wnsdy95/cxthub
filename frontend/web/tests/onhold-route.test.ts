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

// Old bookmarks stay valid, even when another repository is named onhold.
const oldView = parseRoute('/alice/cxthub/-/onhold', '');
assert.equal(resolvedWorkspaceTab(oldView, repos), 'onhold');
assert.equal(findRepositoryByRoute(oldView, repos)?.id, legacy.id);

assert.equal(repoPath(workspace, named), '/alice/cxthub/onhold');
const namedContext = parseRoute('/alice/cxthub/onhold', '');
assert.equal(findRepositoryByRoute(namedContext, repos)?.id, named.id);
assert.equal(resolvedWorkspaceTab(namedContext, repos), undefined);
assert.equal(repoPath(workspace, named, 'onhold'), '/alice/cxthub/onhold/onhold');
const namedView = parseRoute('/alice/cxthub/onhold/onhold', '');
assert.equal(findRepositoryByRoute(namedView, repos)?.id, named.id);
assert.equal(resolvedWorkspaceTab(namedView, repos), 'onhold');

const idView = parseRoute('/w/ws-1', '?tab=onhold');
assert.equal(resolvedWorkspaceTab(idView, repos), 'onhold');
assert.equal(findRepositoryByRoute(idView, repos)?.id, legacy.id);
assert.equal(resolvedWorkspaceTab(parseRoute('/alice/cxthub', '?tab=invalid'), repos), undefined);
assert.equal(parseRoute('/alice/cxthub/-/invalid', ''), null);

console.log('onhold-route: legacy links and repository identity remain distinct');
