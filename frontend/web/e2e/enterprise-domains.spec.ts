import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';

test('domain challenges require owners and preserve revision/audit through real API', async ({ page, playwright }, testInfo) => {
 test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires real backend');
 const errors = capturePageErrors(page); const origin = 'http://127.0.0.1:4174';
 const headers = { Origin: origin, 'X-Cxt-CSRF': '1' }; const owner = page.context().request;
 const member = await playwright.request.newContext({ baseURL: origin, extraHTTPHeaders: headers });
 const suffix = `${Date.now()}-${testInfo.retry}`;
 try {
  for (const [api, email] of [[owner, `domain-owner-${suffix}@example.test`], [member, `domain-admin-${suffix}@example.test`]] as const) {
   expect((await api.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:${email}:Domains` } })).ok()).toBeTruthy();
  }
  await owner.patch('/api/v1/me', { headers, data: { locale: 'en' } });
  const e = await (await owner.post('/api/v1/enterprises', { headers, data: { name: 'Domain Group', slug: `domains-${suffix}` } })).json();
  const admin = await (await member.get('/api/v1/me')).json();
  expect((await owner.put(`/api/v1/enterprises/${e.id}/members/${admin.id}`, { headers, data: { role: 'admin' } })).ok()).toBeTruthy();
  const endpoint = `/api/v1/enterprises/${e.id}/domains`;
  expect((await member.post(endpoint, { data: { domain: 'example.test', revision: '' } })).status()).toBe(403);
  await page.goto(`/enterprises/${e.slug}`);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  const section = page.getByRole('region', { name: 'Verified domains' });
  await expect(section).toContainText('It does not connect accounts, add members or enable SSO.');
  await section.getByLabel('Company domain', { exact: true }).fill('Example.Test');
  await section.getByRole('button', { name: 'Create DNS challenge', exact: true }).click();
  await expect(section.getByLabel('TXT record name', { exact: true })).toHaveValue('_cxthub-verification.example.test');
  const before = (await (await owner.get(endpoint)).json())[0];
  await expect(section.getByLabel('TXT record value', { exact: true })).toHaveValue(before.challenge);
  await section.screenshot({ path: testInfo.outputPath('enterprise-domain-challenge.png') });
  await section.getByRole('button', { name: 'Generate new challenge', exact: true }).click();
  await expect.poll(async () => (await (await owner.get(endpoint)).json())[0].revision).not.toBe(before.revision);
  expect((await owner.post(endpoint + '/release', { headers, data: { domain: before.domain, revision: before.revision } })).status()).toBe(409);
  await section.getByRole('button', { name: 'Release domain claim', exact: true }).click();
  await expect(section).toContainText('No domain claims yet.');
  const audit = await (await owner.get(`/api/v1/enterprises/${e.id}/audit`)).json();
  expect(audit.some((event: { action: string }) => event.action === 'enterprise.domain.released')).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath('enterprise-domain-management.png'), fullPage: true });
  expect(errors).toEqual([]);
 } finally { await member.dispose(); }
});
