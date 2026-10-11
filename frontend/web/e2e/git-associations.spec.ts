import { expect, test, type Page } from '@playwright/test';
import { translate, type Locale } from '../src/i18n';
import type { GitAssociationStatus, GitAssociationStatusPage } from '../src/types';
import { capturePageErrors, installApiFixture, type ApiRequest, type ApiResponse } from './api-fixture';

const repo = 'repo-associations';
const capture = `sha256:${'a'.repeat(64)}`;
const originalHead = 'b'.repeat(40);
const publication = 'c'.repeat(32);
const firstID = 'd'.repeat(64);
const secondID = 'e'.repeat(64);

function item(overrides: Partial<GitAssociationStatus> = {}): GitAssociationStatus {
  return {
    id: firstID, kind: 'integration', state: 'waiting', reason: 'awaiting_bound_head',
    source_branch: 'feature/exact-source', source_branch_id: 'source-incarnation-id',
    destination_branch: 'main', destination_branch_id: 'destination-incarnation-id',
    capture, original_head: originalHead, publication_id: publication,
    admission_id: 'f'.repeat(64), attempts: 3, next_attempt: '2026-10-12T08:00:00Z',
    ...overrides,
  };
}

function result(request: ApiRequest, overrides: Partial<GitAssociationStatusPage> = {}): GitAssociationStatusPage {
  const kind = request.searchParams.get('kind') as GitAssociationStatusPage['kind'];
  return {
    repo_id: repo,
    kind,
    filter: request.searchParams.get('state') as GitAssociationStatusPage['filter'],
    items: [item({ kind, reason: kind === 'memory' ? 'evidence_pending' : 'awaiting_bound_head' })], discovery: { state: 'checkpoint_recorded', version: '9007199254740993' },
    reconciliation: { state: 'waiting', page: 2 }, current_applicability: 'not_evaluated',
    ...overrides,
  };
}

async function fixture(page: Page, respond: (request: ApiRequest) => ApiResponse, locale: Locale = 'en', emptyGraph = false) {
  const calls: ApiRequest[] = [];
  const mutations: string[] = [];
  const errors = capturePageErrors(page);
  page.on('request', request => {
    if (new URL(request.url()).pathname.startsWith('/api/v1/') && !['GET', 'HEAD'].includes(request.method())) mutations.push(`${request.method()} ${request.url()}`);
  });
  await page.addInitScript(value => localStorage.setItem('cxt.locale', value), locale);
  const unexpected = await installApiFixture(page, request => {
    if (request.pathname === `/api/v1/repos/${repo}/git/associations`) {
      calls.push(request);
      expect(request.method).toBe('GET');
      expect(request.searchParams.get('limit')).toBe('30');
      return respond(request);
    }
    if (request.method !== 'GET') return undefined;
    if (request.pathname === '/api/v1/me') return { body: { id: 'alice', username: 'alice', locale } };
    if (request.pathname === '/api/v1/repositories') return { body: [{ id: 'repository-associations', name: 'project', slug: 'project', owner_username: 'alice', visibility: 'private', effective_role: 'viewer' }] };
    if (request.pathname === '/api/v1/repos') return { body: [{ id: repo, default_branch: 'main', context_protocol: 1 }] };
    if (request.pathname.endsWith('/members')) return { body: [{ repository_id: 'repository-associations', user_id: 'alice', role: 'viewer' }] };
    if (request.pathname.endsWith('/view')) return { body: {
      refs: emptyGraph ? [] : [{ repo_id: repo, kind: 'branch', name: 'main', target: capture }],
      snapshots: emptyGraph ? [] : [{ id: capture, repo_id: repo, doc_hash: capture, branch: 'main', provider: 'codex', fidelity: 'full', parents: [], message: 'Current context', created_at: '2026-10-01T00:00:00Z' }],
      pending: [], unsync: [], history: [], reflog: [],
    } };
    if (request.pathname.includes('/docs/')) {
      const envelope = { cir_version: '2', source_provider: 'codex' };
      return { body: request.pathname.endsWith('/events') ? { hash: capture, envelope, events: [], total: 0, offset: 0, next: -1, inherited: 0 } : { cir: { envelope, events: [] } } };
    }
    if (request.pathname.endsWith('/secrets') || request.pathname.includes('/settings/')) return { body: null };
    return undefined;
  });
  await page.goto('/alice/project');
  const panel = page.locator('.git-associations');
  await expect(panel.locator(':scope > summary')).toBeVisible();
  return { panel, calls, errors, unexpected, mutations };
}

test('context linking reads on opening and reopening but never polls while idle or collapsed', async ({ page }) => {
  await page.clock.install({ time: new Date('2026-10-11T00:00:00Z') });
  const state = await fixture(page, request => ({ body: result(request) }));
  await expect(state.panel).not.toHaveAttribute('open');
  expect(state.calls).toHaveLength(0);
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel).toContainText(firstID);
  expect(state.calls).toHaveLength(1);
  expect(Object.fromEntries(state.calls[0].searchParams)).toEqual({ kind: 'integration', state: 'pending', cursor: '', limit: '30' });
  for (const exact of [capture, originalHead, publication, 'source-incarnation-id', 'destination-incarnation-id', '9007199254740993']) {
    await expect(state.panel.locator('code').filter({ hasText: exact })).toHaveText(exact);
  }
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.scope'));
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.historical'));
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.applicability'));
  const lastRead = state.panel.locator('.git-association-last-read time');
  await expect(lastRead).toHaveAttribute('datetime', /2026-10-11T/);
  const initialRead = await lastRead.getAttribute('datetime');
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.readTimeScope'));
  await page.clock.fastForward(120_000);
  await page.evaluate(() => { window.dispatchEvent(new Event('focus')); window.dispatchEvent(new Event('online')); });
  await page.clock.fastForward(1000);
  expect(state.calls).toHaveLength(1);
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel).not.toHaveAttribute('open');
  await page.clock.fastForward(120_000);
  expect(state.calls).toHaveLength(1);
  await state.panel.locator(':scope > summary').click();
  await expect.poll(() => state.calls.length).toBe(2);
  await expect(lastRead).not.toHaveAttribute('datetime', initialRead!);
  await expect(state.panel).toContainText(firstID);
  expect(state.calls[1].searchParams.toString()).toBe(state.calls[0].searchParams.toString());
  await page.clock.fastForward(1000);
  await state.panel.getByRole('button', { name: 'Refresh records' }).click();
  await expect.poll(() => state.calls.length).toBe(3);
  expect(state.calls[2].searchParams.toString()).toBe(state.calls[0].searchParams.toString());
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});

test('context linking pagination is bounded and filter changes discard the old cursor', async ({ page }) => {
  const state = await fixture(page, request => {
    const kind = request.searchParams.get('kind');
    const cursor = request.searchParams.get('cursor');
    const currentID = kind === 'publication' ? publication : cursor ? secondID : firstID;
    return { body: result(request, {
      items: [item({ id: currentID, kind: kind!, state: kind === 'publication' ? 'admitted' : 'waiting', reason: kind === 'publication' ? undefined : 'evidence_pending', destination_branch: kind === 'publication' ? undefined : 'main', destination_branch_id: kind === 'publication' ? undefined : 'destination-incarnation-id' })],
      next_cursor: cursor ? '' : currentID,
    }) };
  });
  await state.panel.locator(':scope > summary').click();
  await state.panel.getByRole('button', { name: 'Next', exact: true }).click();
  await expect(state.panel).toContainText(secondID);
  await expect(state.panel).not.toContainText(firstID);
  expect(state.calls.at(-1)?.searchParams.get('cursor')).toBe(firstID);
  await state.panel.getByLabel('Records to show').selectOption('all');
  await expect(state.panel).toContainText(firstID);
  expect(Object.fromEntries(state.calls.at(-1)!.searchParams)).toEqual({ kind: 'integration', state: 'all', cursor: '', limit: '30' });
  await state.panel.getByRole('button', { name: 'Next', exact: true }).click();
  await expect(state.panel).toContainText(secondID);
  await state.panel.getByLabel('Record type').selectOption('publication');
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.admitted'));
  await expect(state.panel).toContainText('Destination not provided in this response');
  await expect(state.panel).not.toContainText('No destination recorded');
  expect(Object.fromEntries(state.calls.at(-1)!.searchParams)).toEqual({ kind: 'publication', state: 'all', cursor: '', limit: '30' });
  await state.panel.getByLabel('Record type').selectOption('memory');
  await expect(state.panel.locator('li strong')).toHaveText(translate('en', 'promotion.waiting'));
  expect(Object.fromEntries(state.calls.at(-1)!.searchParams)).toEqual({ kind: 'memory', state: 'all', cursor: '', limit: '30' });
  await state.panel.getByRole('button', { name: 'Next', exact: true }).click();
  await expect(state.panel).toContainText(secondID);
  const callsBeforeReopen = state.calls.length;
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel).not.toHaveAttribute('open');
  await state.panel.locator(':scope > summary').click();
  await expect.poll(() => state.calls.length).toBe(callsBeforeReopen + 1);
  expect(Object.fromEntries(state.calls.at(-1)!.searchParams)).toEqual({ kind: 'memory', state: 'all', cursor: firstID, limit: '30' });
  await expect(state.panel.getByRole('button', { name: 'Back to first page' })).toBeEnabled();
  await state.panel.getByRole('button', { name: 'Back to first page' }).click();
  await expect(state.panel).toContainText(firstID);
  await expect(state.panel).not.toContainText(secondID);
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});

for (const locale of ['en', 'ko'] as const) test(`context linking displays authoritative states and safe reasons in ${locale}`, async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 420, height: 900 });
  await page.clock.setFixedTime(new Date('2040-01-01T00:00:00Z'));
  const rows = [
    item({ id: firstID, state: 'retrying', reason: 'lease_expired', next_attempt: '2099-01-01T00:00:00Z' }),
    item({ id: secondID, state: 'processing', reason: undefined, next_attempt: '2000-01-01T00:00:00Z' }),
    item({ id: '1'.repeat(64), state: 'toString', reason: 'secret-provider-debug-response' }),
    item({ id: '2'.repeat(64), state: 'completed', contribution_id: '3'.repeat(64), next_attempt: undefined, reason: undefined }),
  ];
  const state = await fixture(page, request => ({ body: result(request, { items: request.searchParams.get('state') === 'all' ? rows : rows.slice(0, 3), discovery: { state: 'future_state', version: '9' }, reconciliation: { state: 'completed', page: 4 } }) }), locale);
  await state.panel.locator(':scope > summary').click();
  await state.panel.getByLabel(translate(locale, 'gitAssociations.filter')).selectOption('all');
  const list = state.panel.locator('li');
  await expect(list.nth(0).locator('strong')).toHaveText(translate(locale, 'promotion.retrying'));
  await expect(list.nth(0)).toContainText(translate(locale, 'gitAssociations.expiredLease'));
  await expect(list.nth(0).locator('time')).toHaveText('2099-01-01T00:00:00Z');
  await expect(list.nth(1).locator('strong')).toHaveText(translate(locale, 'gitAssociations.processing'));
  await expect(list.nth(1).locator('time')).toHaveAttribute('datetime', '2000-01-01T00:00:00Z');
  await expect(list.nth(2).locator('strong')).toHaveText(translate(locale, 'gitAssociations.unknown'));
  await expect(list.nth(2)).toContainText(translate(locale, 'gitAssociations.unknownReason'));
  await expect(state.panel).not.toContainText('secret-provider-debug-response');
  await expect(list.nth(3).locator('strong')).toHaveText(translate(locale, 'gitAssociations.completed'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.historical'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.applicability'));
  await expect(state.panel).toContainText(translate(locale, 'gitScans.checked'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.lastRead'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.readTimeScope'));
  await expect(state.panel.locator('.git-association-last-read time')).toHaveAttribute('datetime', '2040-01-01T00:00:00.000Z');
  expect(await state.panel.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true);
  await state.panel.screenshot({ path: testInfo.outputPath(`git-associations-${locale}.png`) });
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});

test('unsupported diagnostics remain an error until an explicit read retry succeeds', async ({ page }) => {
  let fail = true;
  const state = await fixture(page, request => fail
    ? { status: 503, body: { error: { code: 'unavailable', message: 'private provider diagnostics' } } }
    : { body: result(request, { items: [], discovery: { state: 'no_origin', version: '0' }, reconciliation: { state: 'unavailable', page: 0 } }) }, 'en', true);
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel.getByRole('alert')).toContainText(translate('en', 'gitAssociations.loadError'));
  await expect(state.panel.locator('.git-association-empty')).toHaveCount(0);
  await expect(state.panel).not.toContainText('private provider diagnostics');
  expect(state.calls).toHaveLength(1);
  fail = false;
  await state.panel.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(state.panel.locator('.git-association-empty')).toHaveText(translate('en', 'gitAssociations.empty'));
  await expect(state.panel.getByRole('alert')).toHaveCount(0);
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.noOrigin'));
  await expect(state.panel).toContainText(translate('en', 'gitAssociations.unavailable'));
  await expect(state.panel).not.toContainText('Provider page 0');
  expect(state.calls).toHaveLength(2);
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});

for (const locale of ['en', 'ko'] as const) test(`reopening replaces no-origin diagnostics without treating missing destinations as absent in ${locale}`, async ({ page }) => {
  let configured = false;
  const state = await fixture(page, request => ({ body: result(request, configured ? {
    discovery: { state: 'checkpoint_recorded', version: '3' }, reconciliation: { state: 'not_started', page: 0 },
    items: [item({ id: publication, kind: 'publication', state: 'admitted', reason: undefined, destination_branch: undefined, destination_branch_id: undefined, next_attempt: undefined })],
  } : { items: [], discovery: { state: 'no_origin', version: '0' }, reconciliation: { state: 'unavailable', page: 0 } }) }), locale, true);
  await state.panel.locator(':scope > summary').click();
  await state.panel.getByLabel(translate(locale, 'gitAssociations.filter')).selectOption('all');
  await state.panel.getByLabel(translate(locale, 'gitAssociations.kind')).selectOption('publication');
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.noOrigin'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.unavailable'));
  await expect(state.panel).not.toContainText(translate(locale, 'gitAssociations.headPage', { page: 0 }));
  const callsBeforeClose = state.calls.length;
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel).not.toHaveAttribute('open');
  configured = true;
  expect(state.calls).toHaveLength(callsBeforeClose);
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.admitted'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.noDestination'));
  await expect(state.panel).toContainText(translate(locale, 'gitAssociations.notStarted'));
  await expect(state.panel).not.toContainText(translate(locale, 'gitAssociations.noOrigin'));
  await expect(state.panel).not.toContainText(translate(locale, 'gitAssociations.headPage', { page: 0 }));
  expect(state.calls).toHaveLength(callsBeforeClose + 1);
  expect(Object.fromEntries(state.calls.at(-1)!.searchParams)).toEqual({ kind: 'publication', state: 'all', cursor: '', limit: '30' });
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});

test('context linking exposes loading and isolates a late response from a previous filter', async ({ page }) => {
  const state = await fixture(page, request => ({ body: result(request) }));
  let release: () => void = () => {};
  const pending = new Promise<void>(resolve => { release = resolve; });
  await page.route('**/git/associations?**', async route => {
    const url = new URL(route.request().url());
    if (url.searchParams.get('kind') !== 'integration') return route.fallback();
    await pending;
    await route.fulfill({ json: result({ method: 'GET', pathname: url.pathname, searchParams: url.searchParams }, { items: [item({ id: '9'.repeat(64) })] }) });
  });
  await state.panel.locator(':scope > summary').click();
  await expect(state.panel.getByRole('status')).toHaveText(translate('en', 'gitAssociations.loading'));
  await expect(state.panel.getByRole('button', { name: 'Refresh records' })).toBeDisabled();
  await state.panel.getByLabel('Record type').selectOption('memory');
  await expect(state.panel).toContainText(firstID);
  release();
  await expect(state.panel.getByLabel('Record type')).toHaveValue('memory');
  await expect(state.panel).not.toContainText('9'.repeat(64));
  expect(state.mutations).toEqual([]);
  expect(state.errors).toEqual([]);
  expect(state.unexpected).toEqual([]);
});
