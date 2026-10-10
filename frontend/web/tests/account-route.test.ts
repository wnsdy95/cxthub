import assert from 'node:assert/strict';
import { accountPath, accountCreationPath, repositoryCreationPath, parseRoute } from '../src/route';

assert.equal(repositoryCreationPath(), '/settings/repositories/new');
assert.deepEqual(parseRoute(repositoryCreationPath(), ''), { kind: 'repositoryCreate' });
assert.deepEqual(parseRoute('/settings/repositories/new/', ''), { kind: 'repositoryCreate' });

for (const section of ['account', 'organizations', 'enterprises'] as const) {
  assert.equal(accountPath(section), `/settings/${section}`);
  assert.deepEqual(parseRoute(accountPath(section), ''), { kind: 'account', section });
  assert.deepEqual(parseRoute(`${accountPath(section)}/`, '?tab=settings'), { kind: 'account', section });
}
for (const section of ['organizations', 'enterprises'] as const) {
  assert.equal(accountCreationPath(section), `/settings/${section}/new`);
  assert.deepEqual(parseRoute(accountCreationPath(section), ''), { kind: 'account', section, create: true });
}
for (const path of ['/settings', '/settings/unknown', '/settings/account/new', '/settings/account/extra', '/settings/organizations/new/extra', '/settings/repositories', '/settings/repositories/new/extra', '/settings/%2e%2e', '/settings/organizations%2fnew']) {
  assert.deepEqual(parseRoute(path, ''), { kind: 'notFound' });
}
assert.deepEqual(parseRoute('/alice', ''), { kind: 'user', username: 'alice' });
assert.deepEqual(parseRoute('/alice/settings', ''), { kind: 'repository', username: 'alice', slug: 'settings' });
assert.deepEqual(parseRoute('/enterprises/acme', ''), { kind: 'enterprise', slug: 'acme' });
