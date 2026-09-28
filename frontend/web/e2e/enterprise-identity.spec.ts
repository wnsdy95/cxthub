import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';

test('identity UI reports availability and clears secrets after a revisioned save', async ({ page }, testInfo) => {
 test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires real base identity API');
 await page.setViewportSize({ width: 1280, height: 1400 });
 const errors = capturePageErrors(page); const origin = 'http://127.0.0.1:4174';
 const headers = { Origin: origin, 'X-Cxt-CSRF': '1' }; const owner = page.context().request;
 const suffix = `${Date.now()}-${testInfo.retry}`;
 expect((await owner.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:oidc-owner-${suffix}@example.test:Identity` } })).ok()).toBeTruthy();
 await owner.patch('/api/v1/me', { headers, data: { locale: 'en' } });
 const e = await (await owner.post('/api/v1/enterprises', { headers, data: { name: 'Identity Group', slug: `identity-${suffix}` } })).json();
 await page.goto(`/enterprises/${e.slug}?tab=identity`);
 const section = page.getByRole('region', { name: 'Enterprise identity' });
 await expect(section).toContainText('Your operator has not enabled identity connections yet.');
 await expect(section.getByRole('button', { name: 'Verify with identity provider' })).toHaveCount(0);
 // UI interaction fixtures only. Signed callbacks, authorization and replay use
 // the real provider adapter/API/PostgreSQL in flow_pg_test.go.
 const endpoint = `/api/v1/enterprises/${e.id}/oidc`;
 let connection: Record<string, string> | undefined;
 let submitted: Record<string, string> | undefined;
 await page.route(`**${endpoint}`, async (route) => {
  if (route.request().method() === 'POST') {
   submitted = route.request().postDataJSON();
   expect(submitted!.revision).toBe('');
   expect(submitted!.client_secret).toBe('synthetic-browser-secret');
   connection = { enterprise_id: e.id, domain: submitted!.domain, issuer: submitted!.issuer, client_id: submitted!.client_id, auth_method: submitted!.auth_method, revision: 'oc_fixture' };
  }
  await route.fulfill({ json: { available: true, configured: Boolean(connection), callback_uri: `${origin}/api/v1/auth/enterprise/oidc/callback`, connection, linked: false } });
 });
 await page.reload();
 await section.getByText('Configure OpenID Connect', { exact: true }).click();
 await section.getByLabel('Verified company domain', { exact: true }).fill('example.test');
 await section.getByLabel('Issuer URL', { exact: true }).fill('https://issuer.example.test');
 await section.getByLabel('Client ID', { exact: true }).fill('fixture-client');
 await section.getByLabel('Client secret', { exact: true }).fill('synthetic-browser-secret');
 await section.getByRole('button', { name: 'Save identity connection' }).click();
 await expect(section).toContainText('https://issuer.example.test');
 await section.getByText('Configure OpenID Connect', { exact: true }).click();
 await expect(section.getByLabel('Client secret', { exact: true })).toHaveValue('');
 await expect(section).toContainText('This browser session has not been verified.');
 await expect(section).toContainText('This does not grant membership or enable mandatory SSO.');
 await section.screenshot({ path: testInfo.outputPath('enterprise-identity.png') });
 expect(submitted?.domain).toBe('example.test');
 expect(errors).toEqual([]);
});

test('SAML settings keep metadata edits revisioned and show supported scope', async ({ page }, testInfo) => {
 test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires real base identity API');
 await page.setViewportSize({ width: 1280, height: 1400 });
 const errors = capturePageErrors(page); const origin = 'http://127.0.0.1:4174';
 const headers = { Origin: origin, 'X-Cxt-CSRF': '1' }; const owner = page.context().request;
 const suffix = `${Date.now()}-${testInfo.retry}`;
 expect((await owner.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:saml-owner-${suffix}@example.test:SAML` } })).ok()).toBeTruthy();
 await owner.patch('/api/v1/me', { headers, data: { locale: 'en' } });
 const e = await (await owner.post('/api/v1/enterprises', { headers, data: { name: 'SAML Group', slug: `saml-${suffix}` } })).json();
 await page.goto(`/enterprises/${e.slug}?tab=identity`);
 const section = page.getByRole('region', { name: 'SAML 2.0 identity' });
 await expect(section).toContainText('Your operator has not enabled identity connections yet.');
 const endpoint = `/api/v1/enterprises/${e.id}/saml`;
 let connection: Record<string, string> | undefined; let submitted: Record<string, string> | undefined;
 await page.route(`**${endpoint}`, async route => {
  if (route.request().method() === 'POST') {
   submitted = route.request().postDataJSON();
   expect(submitted!.revision).toBe('');
   connection = { enterprise_id: e.id, domain: submitted!.domain, issuer: 'https://idp.example.test', revision: 'sc_fixture', certificate: 'synthetic-public-certificate' };
  }
  await route.fulfill({ json: { available: true, configured: Boolean(connection), connection, entity_id: `${origin}/api/v1/auth/enterprise/saml/${e.id}/metadata`, acs: `${origin}/api/v1/auth/enterprise/saml/${e.id}/acs`, linked: false } });
 });
 await page.reload(); await section.getByText('Configure SAML', { exact: true }).click();
 await section.getByLabel('Verified company domain', { exact: true }).fill('example.test');
 await section.getByLabel('Identity provider metadata XML').fill('<EntityDescriptor>synthetic UI fixture</EntityDescriptor>');
 await section.getByRole('button', { name: 'Save identity connection' }).click();
 await expect(section).toContainText('https://idp.example.test');
 await section.getByText('Configure SAML', { exact: true }).click();
 await expect(section.getByLabel('Identity provider metadata XML')).toHaveValue('');
 await expect(section).toContainText('Unsolicited login, encrypted assertions and single logout are not supported.');
 await expect(section.getByRole('link', { name: 'Open service provider metadata' })).toHaveAttribute('href', `${origin}/api/v1/auth/enterprise/saml/${e.id}/metadata`);
 await section.screenshot({ path: testInfo.outputPath('enterprise-saml.png') });
 expect(submitted?.metadata).toContain('synthetic UI fixture'); expect(errors).toEqual([]);
});
