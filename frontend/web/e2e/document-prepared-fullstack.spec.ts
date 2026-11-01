import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { expect, test, type APIRequestContext } from '@playwright/test';
import type { DocFindPage } from '../src/documentFind';

const origin = 'http://127.0.0.1:4174';
const headers = { Origin: origin, 'X-Cxt-CSRF': '1', 'X-Cxt-Doc-Identities': 'cxt-manifest-sha256-v1' };
const hash = (value: string) => `sha256:${createHash('sha256').update(value).digest('hex')}`;
let fixtureBinary: string | undefined;

function documentFixture(input: unknown): { legacy: { hash: string; cir: unknown }; root: { hash: string; identity: string }; chunks: unknown[] } {
  if (!fixtureBinary) {
    const folder = mkdtempSync(path.join(tmpdir(), 'cxt-conversation-e2e-'));
    fixtureBinary = path.join(folder, 'conversation');
    execFileSync('go', ['build', '-tags=conversationfixture', '-o', fixtureBinary, './internal/testsupport/conversationfixture'],
      { cwd: path.resolve('..', '..', 'backend') });
    process.once('exit', () => rmSync(folder, { recursive: true, force: true }));
  }
  return JSON.parse(execFileSync(fixtureBinary, [], { input: JSON.stringify(input), encoding: 'utf8', maxBuffer: 8 << 20 }));
}

async function login(client: APIRequestContext, email: string) {
  const response = await client.post('/api/v1/auth/session', { headers: { ...headers, Authorization: `Bearer dev:${email}:Prepared Find` } });
  expect(response.ok(), await response.text()).toBe(true);
  const locale = await client.patch('/api/v1/me', { headers, data: { locale: 'en' } });
  expect(locale.ok(), await locale.text()).toBe(true);
  return (await client.get('/api/v1/me')).json();
}

for (const legacy of [false, true]) test(`real prepared display find reaches late text and rechecks revocation: legacy=${legacy}`, async ({ page, playwright }, info) => {
  test.skip(!process.env.CXT_E2E_FULLSTACK, 'requires owned PostgreSQL, actual API and worker');
  test.setTimeout(120_000);
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  const suffix = `${Date.now()}-${info.retry}-${legacy}`;
  const owner = await playwright.request.newContext({ baseURL: origin, extraHTTPHeaders: headers });
  try {
    const ownerUser = await login(owner, `prepared-owner-${suffix}@example.test`);
    const readerUser = await login(page.request, `prepared-reader-${suffix}@example.test`);
    const created = await owner.post('/api/v1/repositories', { data: { name: `Prepared-${legacy}-${suffix}` } });
    expect(created.ok(), await created.text()).toBe(true);
    const metadata = await created.json();
    const remote = `http://cxthub.test/${ownerUser.username}/${metadata.slug}`;
    const repo = hash(remote.replace('http://', '').toLowerCase());
    const registered = await owner.post('/api/v1/repos', { data: { id: repo, remote_url: remote, default_branch: 'main' } });
    expect(registered.ok(), await registered.text()).toBe(true);
    const base = `/api/v1/repos/${encodeURIComponent(repo)}`;
    if (!legacy) {
      const upgraded = await owner.patch(`${base}/about`, { data: { required_doc_identity: 'cxt-manifest-sha256-v1' } });
      expect(upgraded.ok(), await upgraded.text()).toBe(true);
    }
    const document = documentFixture({ envelope: { cir_version: '2', source_provider: 'codex', source_model: 'test',
      captured_at: '2026-10-22T00:00:00Z', cwd: '', git_branch: 'main', session_origin_id: suffix, fidelity: 'full' }, events: [
      { kind: 'message', seq: 0, role: 'user', blocks: [{ type: 'text', text: 'Prepared search begins here' }] },
      { kind: 'tool_result', seq: 1, call_id: 'large-result', output: 'x'.repeat(600_000) + 'NEEDLE🙂' + 'y'.repeat(120_000) },
      { kind: 'message', seq: 2, role: 'assistant', blocks: [{ type: 'text', text: 'Prepared search finishes here' }] },
    ] });
    const documentHash = legacy ? document.legacy.hash : document.root.hash;
    if (!legacy) {
      const chunks = await owner.post(`${base}/push/chunks`, { data: { chunks: document.chunks } });
      expect(chunks.ok(), await chunks.text()).toBe(true);
      const submitted = await owner.post(`${base}/push/doc-jobs`, { data: document.root });
      expect(submitted.status(), await submitted.text()).toBe(202);
      const receipt = await submitted.json();
      await expect.poll(async () => {
        const status = await owner.get(`${base}/push/doc-jobs/${encodeURIComponent(receipt.id)}`);
        expect(status.ok(), await status.text()).toBe(true);
        return (await status.json()).state;
      }, { timeout: 30_000 }).toBe('completed');
    }
    const published = await owner.post(`${base}/push/objects`, { data: {
      ...(legacy ? { docs: [document.legacy] } : {}), snapshots: [{ id: documentHash, doc_hash: documentHash,
        ...(legacy ? {} : { doc_identity: 'cxt-manifest-sha256-v1' }), repo_id: repo, branch: 'main', provider: 'codex',
        session_id: suffix, fidelity: 'full', parents: [], message: 'Prepared search fixture', created_at: '2026-10-22T00:00:00Z' }],
    } });
    expect(published.ok(), await published.text()).toBe(true);
    const reference = await owner.put(`${base}/refs/branch/main`, { data: { target: documentHash, expected_target: '' } });
    expect(reference.ok(), await reference.text()).toBe(true);
    const invited = await owner.post(`/api/v1/repositories/${metadata.id}/invites`, { data: { email: readerUser.email, role: 'viewer', expires_in_days: 1 } });
    expect(invited.ok(), await invited.text()).toBe(true);
    const invitation = await invited.json();
    const accepted = await page.request.post(`/api/v1/invites/${encodeURIComponent(invitation.token)}/accept`, { headers });
    expect(accepted.ok(), await accepted.text()).toBe(true);
    const responses: DocFindPage[] = [];
    const requests: URL[] = [];
    const responseCaptures: Promise<void>[] = [];
    const responseErrors: string[] = [];
    page.on('request', request => {
      const url = new URL(request.url());
      if (url.pathname.endsWith('/find') || url.pathname.endsWith('/find-fragments')) requests.push(url);
    });
    page.on('requestfinished', request => {
      if (!new URL(request.url()).pathname.endsWith('/find')) return;
      responseCaptures.push((async () => {
        const response = await request.response();
        if (response?.ok()) responses.push(await response.json());
      })().catch(error => { responseErrors.push(String(error)); }));
    });
    await page.goto(`/${ownerUser.username}/${metadata.slug}`);
    await expect(page.locator('.viewer')).toContainText('Prepared search finishes here', { timeout: 30_000 });
    await page.getByRole('button', { name: 'Find in document', exact: true }).click();
    await page.getByRole('combobox', { name: 'Search representation' }).selectOption('display');
    await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
    await page.getByRole('button', { name: 'Search', exact: true }).click();
    await expect(page.locator('.document-find-preview mark')).toHaveText('NEEDLE🙂', { timeout: 60_000 });
    await expect.poll(() => responses.flatMap(response => response.matches).length).toBeGreaterThan(0);
    await Promise.all(responseCaptures);
    expect(responseErrors).toEqual([]);
    const match = responses.flatMap(response => response.matches)[0];
    expect(match.event_index).toBe(1);
    expect(match.byte_offset).toBeGreaterThan(512 << 10);
    expect(match.prepared_page).toBeGreaterThan(0);
    expect(responses.every(response => response.projection === (legacy ? 'find-legacy-display-prepared-v1' : 'find-display-prepared-v1'))).toBe(true);
    expect(requests.every(request => request.searchParams.get('representation') === 'display-prepared-v1')).toBe(true);
    await page.getByRole('button', { name: 'Read the matching conversation', exact: true }).click();
    await expect(page.locator('.document-find-source mark')).toHaveText('NEEDLE🙂');
    expect(requests.find(request => request.pathname.endsWith('/find-fragments'))?.searchParams.get('offset')).toBe(String(match.source_range!.from));
    const revoked = await owner.delete(`/api/v1/repositories/${metadata.id}/members/${encodeURIComponent(readerUser.id)}`);
    expect(revoked.ok(), await revoked.text()).toBe(true);
    const denied = page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/find-fragments') && response.status() === 403);
    await page.getByRole('button', { name: 'Next fragment', exact: true }).click();
    await denied;
    await expect(page.locator('.document-find-reveal [role="alert"]')).toBeVisible();
    await expect(page.locator('.document-find-source')).toHaveCount(0);
    expect(requests.filter(request => request.pathname.endsWith('/find')).every(request => request.searchParams.get('representation') === 'display-prepared-v1')).toBe(true);
    await Promise.all(responseCaptures);
    expect(responseErrors).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await owner.dispose();
  }
});
