import assert from 'node:assert/strict';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { I18nProvider } from '../src/i18n/index.tsx';
import { RoleCapabilities } from '../src/components/RoleCapabilities.tsx';
import { ROLE_CAPABILITIES, ROLES, atLeast } from '../src/roles.ts';
import {
  STORAGE_PRICING,
  billableStorageGiB,
  estimateMonthlyStorageUsd,
  normalizedAverageStorageGiB,
} from '../src/pricing.ts';
import { parseRoute } from '../src/route.ts';

assert.deepEqual(ROLE_CAPABILITIES, [
  { id: 'viewContext', minimumRole: 'viewer' },
  { id: 'pullTeamAssets', minimumRole: 'puller' },
  { id: 'pushContext', minimumRole: 'member' },
  { id: 'manageTeamAssets', minimumRole: 'maintainer' },
  { id: 'administerWorkspace', minimumRole: 'owner' },
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
assert.equal(STORAGE_PRICING.includedGiB, 10);
assert.equal(STORAGE_PRICING.overageUsdPerGiBMonth, 0.07);
assert.equal(normalizedAverageStorageGiB(Number.NaN), 0);
assert.equal(normalizedAverageStorageGiB(-5), 0);
assert.equal(billableStorageGiB(10), 0);
assert.equal(billableStorageGiB(25), 15);
assert.equal(estimateMonthlyStorageUsd(25), 1.05);
