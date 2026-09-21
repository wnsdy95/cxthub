import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nProvider } from '../src/i18n/index.tsx';
import { RoleCapabilities } from '../src/components/RoleCapabilities.tsx';
import { ROLE_CAPABILITIES, ROLES, atLeast } from '../src/roles.ts';
import { parseRoute, repoPath, repositorySlug, repositoryPath } from '../src/route.ts';

assert.deepEqual(ROLE_CAPABILITIES, [
  { id: 'viewContext', minimumRole: 'viewer' },
  { id: 'pullTeamAssets', minimumRole: 'puller' },
  { id: 'pushContext', minimumRole: 'member' },
  { id: 'manageTeamAssets', minimumRole: 'maintainer' },
  { id: 'administerRepository', minimumRole: 'owner' },
]);

for (const [capabilityIndex, capability] of ROLE_CAPABILITIES.entries()) {
  for (const [roleIndex, role] of ROLES.entries()) {
    assert.equal(
      atLeast(role, capability.minimumRole),
      roleIndex >= capabilityIndex,
      `${role} cumulative access for ${capability.id}`,
    );
  }
}

const matrix = renderToStaticMarkup(
  createElement(I18nProvider, null, createElement(RoleCapabilities)),
);
assert.match(matrix, /class="role-capabilities"/);
assert.equal((matrix.match(/class="allowed"/g) ?? []).length, 15, 'cumulative five-role matrix has 15 grants');
assert.equal((matrix.match(/class="denied"/g) ?? []).length, 10, 'cumulative five-role matrix has 10 denials');
for (const role of ROLES) assert.match(matrix, new RegExp(`<code>${role}</code>`));

assert.deepEqual(parseRoute('/pricing'), { kind: 'pricing' });
assert.deepEqual(parseRoute('/oauth/client'), { kind: 'notFound' }, 'server-owned OAuth path is not interpreted as a Repository');
assert.deepEqual(parseRoute('/mcp'), { kind: 'notFound' }, 'server-owned MCP path is not interpreted as a user Namespace');
assert.deepEqual(parseRoute('/connect/mcp'), { kind: 'mcpConsent', request: '' });
assert.deepEqual(parseRoute('/acme/platform/backend'), {
  kind: 'repository',
  username: 'acme',
  slug: 'platform/backend',
});
assert.deepEqual(parseRoute('/acme/platform/-/settings'), {
  kind: 'notFound',
});
assert.deepEqual(parseRoute('/acme/platform/settings'), {
  kind: 'repository',
  username: 'acme',
  slug: 'platform/settings',
});
const repository = { id: 'ws_1', owner_username: 'acme', slug: 'platform' };
assert.equal(repositoryPath(repository, 'members'), '/acme/platform?tab=members');
assert.equal(repositorySlug({ remote_url: 'https://cxthub.com/acme/platform/backend' }), 'backend');
assert.equal(repoPath(repository, { remote_url: 'https://cxthub.com/acme/platform/backend' }), '/acme/platform');
assert.equal(
  repoPath(repository, { remote_url: 'https://cxthub.com/acme/platform/backend' }, 'onhold'),
  '/acme/platform?tab=onhold',
);
assert.equal(
  repoPath(repository, { remote_url: 'https://cxthub.com/acme/platform' }),
  '/acme/platform',
  'legacy two-segment repository keeps its stable repository URL',
);
