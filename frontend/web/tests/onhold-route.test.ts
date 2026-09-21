import assert from 'node:assert/strict';
import { findRepositoryByRoute, parseRoute, repoPath, resolvedRepositoryTab, repositoryPath, enterprisePath } from '../src/route';
import type { Repo } from '../src/types';
const repository = { id: 'repository-1', owner_username: 'alice', slug: 'cxthub' };
const stable = { id: 'stable', remote_url: 'https://cxthub.com/alice/old-container/old-child' } as Repo;
assert.equal(repoPath(repository, stable), '/alice/cxthub', 'display address does not derive from the stable connection URL');
assert.equal(repoPath(repository, stable, 'onhold'), '/alice/cxthub?tab=onhold');
for (const tab of ['members', 'connections', 'settings', 'onhold'] as const) {
 assert.equal(repositoryPath(repository, tab), `/alice/cxthub?tab=${tab}`);
 const route = parseRoute('/alice/cxthub', `?tab=${tab}`);
 assert.equal(resolvedRepositoryTab(route, [stable]), tab);
 assert.equal(findRepositoryByRoute(route, [stable])?.id, stable.id);
 assert.equal(findRepositoryByRoute(route, [stable, { ...stable, id: 'other' }]), undefined, 'ambiguous bindings are not guessed');
 assert.deepEqual(parseRoute(`/alice/cxthub/-/${tab}`, ''), { kind: 'notFound' });
 assert.deepEqual(parseRoute(`/w/repository-1/-/${tab}`, ''), { kind: 'notFound' });
 assert.equal(resolvedRepositoryTab(parseRoute(`/alice/${tab}`, ''), [stable]), undefined, 'repository names never become tabs');
}
assert.deepEqual(parseRoute('/alice/old-container/old-child', '?tab=onhold'), { kind: 'repository', username: 'alice', slug: 'old-container/old-child', tab: 'onhold' });
assert.deepEqual(parseRoute('/alice/cxthub/%2D/settings', ''), { kind: 'notFound' });
assert.deepEqual(parseRoute('/alice/%2e%2e', ''), { kind: 'notFound' });
assert.deepEqual(parseRoute('/alice/a%2fb', ''), { kind: 'notFound' });
assert.deepEqual(parseRoute('/enterprises/acme', ''), { kind: 'enterprise', slug: 'acme' });
assert.equal(enterprisePath('acme'), '/enterprises/acme');
console.log('repository routes: canonical owner/name, server-resolved aliases, query tabs, enterprise accounts');
