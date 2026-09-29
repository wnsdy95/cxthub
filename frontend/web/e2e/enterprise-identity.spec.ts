import { expect, test } from '@playwright/test';
import { capturePageErrors } from './api-fixture';
import type { SAMLConnection } from '../src/federation';
import type { CredentialAssurancesView } from '../src/federation';

test('credential approvals require recent verification, isolate a target and recover from stale state', async ({ page }, testInfo) => {
 test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires PostgreSQL identity API');
 const errors = capturePageErrors(page); const origin = 'http://127.0.0.1:4174';
 const headers = { Origin: origin, 'X-Cxt-CSRF': '1' }; const client = page.context().request;
 const suffix = `${Date.now()}-${testInfo.retry}`;
 expect((await client.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:assurance-${suffix}@example.test:Assurance` } })).ok()).toBeTruthy();
 await client.patch('/api/v1/me', { headers, data: { locale: 'en' } });
 const e = await (await client.post('/api/v1/enterprises', { headers, data: { name: 'Assurance Group', slug: `assurance-${suffix}` } })).json();
 for (let i = 0; i < 2; i++) expect((await client.post('/api/v1/me/cli-tokens', { headers })).ok()).toBeTruthy();
 const endpoint = `/api/v1/enterprises/${e.id}/credential-assurances`;
 const inventory = await client.get(endpoint); expect(inventory.ok()).toBeTruthy();
 let view = await inventory.json() as CredentialAssurancesView;
 expect(view.credentials).toHaveLength(2); expect(view.browser_proof).toBeUndefined();
 expect(new Set(view.credentials.map(c => c.id)).size).toBe(2);
 for (const c of view.credentials) expect(c.id).toMatch(/^cred_[0-9a-f]{32}$/);
 expect(JSON.stringify(view)).not.toContain('tkh_');
 await page.setViewportSize({ width: 1280, height: 1200 });
 await page.goto(`/enterprises/${e.slug}?tab=identity`);
 const section = page.getByRole('region', { name: 'CLI and MCP identity approvals' });
 await expect(section.getByRole('button', { name: 'Approve this connection' }).first()).toBeDisabled();
 await section.getByText('Maximum approval duration', { exact: true }).click();
 await section.getByLabel('Hours', { exact: true }).fill('1');
 await section.getByRole('button', { name: 'Save duration and invalidate previous approvals' }).click();
 await expect.poll(async () => ((await (await client.get(endpoint)).json()) as CredentialAssurancesView).policy?.max_age_hours).toBe(1);
 view = await (await client.get(endpoint)).json() as CredentialAssurancesView;
 // UI transitions use an explicit transport fixture. Signed SAML approval
 // and mutation/race authorization are exercised against real PostgreSQL in Go.
 const proof = { protocol: 'saml' as const, connection_revision: 'sc_fixture', policy_revision: view.policy!.revision };
 view.browser_proof = proof; view.approval_available_until = new Date(Date.now() + 60_000).toISOString();
 await page.route(`**${endpoint}`, route => route.fulfill({ json: view }));
 const target = view.credentials[0].id; const sibling = view.credentials[1].id; let stale = true;
 await page.route(`**${endpoint}/${target}`, async route => {
  expect(route.request().method()).toBe('POST'); expect(route.request().postDataJSON()).toEqual(proof);
  if (stale) { delete view.browser_proof; await route.fulfill({ status: 409, json: { error: { code: 'conflict', message: 'Verification changed. Refresh and verify again.' } } }); return; }
  view.credentials[0] = { ...view.credentials[0], state: 'approved', protocol: 'saml', authenticated_at: new Date().toISOString(), verified_until: new Date(Date.now() + 3_600_000).toISOString() };
  await route.fulfill({ json: { status: 'approved' } });
 });
 await page.route(`**${endpoint}/${target}/revoke`, async route => {
  view.credentials[0] = { ...view.credentials[0], state: 'revoked' }; await route.fulfill({ json: { status: 'revoked' } });
 });
 await page.reload();
 const row = section.getByRole('listitem').filter({ hasText: target });
 await row.getByRole('button', { name: 'Approve this connection' }).click();
 await expect(section.getByRole('alert')).toContainText('Verification changed');
 await expect(row.getByRole('button', { name: 'Approve this connection' })).toBeDisabled();
 stale = false; view.browser_proof = proof;
 await section.getByRole('button', { name: 'Refresh connections' }).click();
 await row.getByRole('button', { name: 'Approve this connection' }).click();
 await expect(row).toContainText('Identity approved');
 await expect(section.getByRole('listitem').filter({ hasText: sibling })).toContainText('Not approved');
 await row.getByRole('button', { name: 'Revoke approval' }).click();
 await expect(row).toContainText('Approval revoked');
 await page.setViewportSize({ width: 390, height: 844 });
 await section.scrollIntoViewIfNeeded();
 expect(await section.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
 await section.screenshot({ path: testInfo.outputPath('credential-assurances.png') });
 expect(errors).toEqual([]);
});

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
 let connection: SAMLConnection | undefined; let submitted: Record<string, string> | undefined;
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
 let revision = 0;
 await page.route(`**${endpoint}/signing`, async route => {
  const input = route.request().postDataJSON(); expect(input.revision).toBe(connection!.revision);
  connection = { ...connection!, revision: `sc_signing_${++revision}` };
  if (input.action === 'prepare') connection.rotation = { id: 'sr_fixture', state: 'prepared', certificate: 'next-public', created_at: new Date().toISOString() };
  else if (input.action === 'activate') {
   expect(input.trust_confirmed).toBe(true);
   connection.certificate = 'next-public';
   connection.rotation = { ...connection.rotation!, state: 'active', certificate: 'old-public', activated_at: new Date().toISOString() };
  } else if (input.action === 'retire') { expect(connection.rotation?.verified_at).toBeTruthy(); delete connection.rotation; }
  else throw new Error(`Unexpected action ${input.action}`);
  await route.fulfill({ json: { available: true, configured: true, connection, entity_id: `${origin}/api/v1/auth/enterprise/saml/${e.id}/metadata`, acs: `${origin}/api/v1/auth/enterprise/saml/${e.id}/acs`, linked: false } });
 });
 const signing = section.getByRole('region', { name: 'Request signing certificate' });
 await signing.getByRole('button', { name: 'Prepare new certificate' }).click();
 await expect(signing.getByRole('button', { name: 'Activate new certificate' })).toBeDisabled();
 await signing.getByRole('checkbox').check();
 await signing.getByRole('button', { name: 'Activate new certificate' }).click();
 await expect(signing.getByRole('button', { name: 'Retire previous key' })).toBeDisabled();
 await expect(signing.getByRole('button', { name: 'Restore previous certificate' })).toBeEnabled();
 // This simulates the query after the real signed callback, exercised in PG tests.
 connection!.rotation!.verified_at = new Date().toISOString();
 await page.reload();
 await expect(signing).toContainText('This does not prove that the provider enforces request signatures.');
 await signing.getByRole('button', { name: 'Retire previous key' }).click();
 await expect(signing.getByRole('button', { name: 'Prepare new certificate' })).toBeVisible();
 expect(connection!.rotation).toBeUndefined();
 await section.screenshot({ path: testInfo.outputPath('enterprise-saml.png') });
 expect(submitted?.metadata).toContain('synthetic UI fixture'); expect(errors).toEqual([]);
});
