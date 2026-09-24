import { expect, test } from '@playwright/test';
import { capturePageErrors, installApiFixture } from './api-fixture';

test('GitHub repository and optional team mapping keep separate choices and generation', async ({ page }, testInfo) => {
 const errors = capturePageErrors(page), mutations: unknown[] = [];
 const connection = { namespace_id: 'ns_org', generation: 4, enabled: true, status: 'connected', checked_at: '2026-09-25T00:00:00Z', next_sync: '2099-01-01T00:00:00Z', installation: { id: 17, account_id: 21, login: 'acme', kind: 'Organization', suspended: false }, repositories: [{ id: 25, account_id: 21, full_name: 'acme/api', private: true }], bindings: [], teams: [{ id: 8, slug: 'backend', name: 'Backend' }], mappings: [], unresolved_members: 0 };
 const unexpected = await installApiFixture(page, ({ method, pathname }) => {
  if (pathname === '/api/v1/me') return { body: { id: 'owner', email: 'owner@example.test', name: 'Owner', username: 'owner', locale: 'en' } };
  if (pathname === '/api/v1/github/connections') return { body: { enabled: true, identity: { external_id: 99, login: 'operator' }, owners: [{ namespace: { id: 'ns_org', slug: 'acme', kind: 'organization' }, connection, repositories: [{ id: 'local-api', slug: 'api' }], context_repos: [{ id: 'context-id', repository_id: 'local-api', git_remote_url: 'https://github.com/acme/api' }], teams: [{ id: 'team-backend', name: 'Platform' }] }] } };
  if (method === 'POST' && pathname === '/api/v1/github/connections/ns_org') return { body: { saved: true } };
 });
 page.on('request', r => { if (r.method() === 'POST' && r.url().endsWith('/github/connections/ns_org')) mutations.push(r.postDataJSON()); });
 await page.goto('/connect/github');
 await expect(page.getByRole('heading', { name: 'GitHub connections', exact: true })).toBeVisible();
 await expect(page.getByLabel('Synchronize team members')).not.toBeChecked();
 await page.getByRole('combobox', { name: 'GitHub repository', exact: true }).selectOption('25');
 await page.getByRole('combobox', { name: 'CXTHub repository', exact: true }).selectOption('context-id');
 await page.getByRole('button', { name: 'Save connection', exact: true }).first().click();
 await expect.poll(() => mutations.length).toBe(1);
 expect(mutations[0]).toEqual({ generation: 4, action: 'bind', binding: { repository_id: 'local-api', context_repo_id: 'context-id', external_id: 25 } });
 await page.getByRole('combobox', { name: 'GitHub team', exact: true }).selectOption('8');
 await page.getByRole('combobox', { name: 'CXTHub team', exact: true }).selectOption('team-backend');
 await page.getByRole('button', { name: 'Save connection', exact: true }).last().click();
 await expect.poll(() => mutations.length).toBe(2);
 expect(mutations[1]).toMatchObject({ generation: 4, action: 'map-team', mapping: { sync_members: false } });
 await page.evaluate(() => window.scrollTo(0, 0));
 await page.screenshot({ path: testInfo.outputPath('github-connections.png'), fullPage: true });
 await page.setViewportSize({ width: 390, height: 844 });
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
 expect(unexpected).toEqual([]); expect(errors).toEqual([]);
});
test('unconfigured GitHub App reports configuration status instead of a dead install button', async ({ page }) => {
 await installApiFixture(page, ({ pathname }) => {
  if (pathname === '/api/v1/me') return { body: { id: 'owner', username: 'owner', locale: 'en' } };
  if (pathname === '/api/v1/github/connections') return { body: { enabled: false, identity: null, owners: [] } };
 });
 await page.goto('/connect/github');
 await expect(page.getByText('GitHub App connections are not enabled on this server yet.')).toBeVisible();
 await expect(page.getByRole('button', { name: 'Connect GitHub App', exact: true })).toHaveCount(0);
});
