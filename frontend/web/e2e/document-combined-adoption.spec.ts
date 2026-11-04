import { expect, test, type Page } from '@playwright/test';
import type { QueryClient } from '@tanstack/react-query';
import type { DocViewEventPage } from '../src/api';
import type { DocFindFragmentPage, DocFindPage } from '../src/documentFind';
import type { DocVisiblePage } from '../src/documentVisible';
import { DOCUMENT_BODY_CACHE_BYTES, DOCUMENT_BODY_CACHE_PAGES } from '../src/documentPageCache';
import { heavyVisiblePage, visibleMessage } from '../tests/documentVisibleFixture';
import { fragmentBytes, fragmentText, fragmentTotalBytes, memoryFold, rawResult } from '../tests/documentActiveFixture';

const documentPath = '/api/v1/repos/repo/docs/document';
const legacyPageSize = 20;
const documentWindowEnabled = process.env.CXT_DOCUMENT_WINDOW === '1' || process.env.CXT_DOCUMENT_BASELINE === '1';

async function harness(page: Page, mode = 'chat', responsive = false) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/__document-combined-adoption*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><head>
<meta name="viewport" content="width=device-width, initial-scale=1" /></head><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  const parameters = new URLSearchParams({ mode });
  if (responsive) parameters.set('responsive', '1');
  await page.goto(`/__document-combined-adoption?${parameters}`);
  await expect(page.locator('[data-document-page="units:0"]').first()).toHaveAttribute('data-page-mounted', 'true');
  return errors;
}

function visiblePage(from: number, pageCount: number, generation: string): DocVisiblePage {
  const body = heavyVisiblePage(from, pageCount * 620);
  return { ...body, generation, replay_cursor: `${generation}-main-${from}`,
    next_cursor: body.next_cursor === null ? null : `${generation}-${body.next_cursor}`,
    units: body.units.map(unit => ({ ...unit,
      event: { ...unit.event!, blocks: [{ type: 'text', text: `${generation}: ${unit.event!.blocks![0].text}` }] },
      fold: { ...unit.fold!, cursor: `${generation}-${unit.fold!.cursor}` },
    })) };
}

function legacyPage(offset: number, pageCount: number): DocViewEventPage {
  const total = pageCount * legacyPageSize;
  const events = Array.from({ length: Math.min(legacyPageSize, total - offset) }, (_unused, position) => {
    const index = offset + position;
    if (index === 1) return { kind: 'tool_result', event_index: index, output: rawResult(index) };
    if (index === 2) return { kind: 'tool_result', event_index: index, deferred: true, event_bytes: fragmentTotalBytes };
    return visibleMessage(index, `Legacy anchor ${index}\n\n` + 'A readable paragraph preserves the scroll window.\n\n'.repeat(6),
      index % legacyPageSize === 0 ? 'user' : 'assistant');
  });
  return { hash: 'document', doc_identity: '', envelope: { cir_version: '2', source_provider: 'codex' },
    total, inherited: 0, offset, next: offset + events.length === total ? -1 : offset + events.length, events };
}

async function transport(page: Page, pageCount: number, legacy = false) {
  const requests: URL[] = [];
  const unexpected: string[] = [];
  const state = { generation: 'generation-one', expired: false };
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    const parameters = url.searchParams;
    requests.push(url);
    const reject = () => {
      unexpected.push(`${route.request().method()} ${url.pathname}${url.search}`);
      return route.fulfill({ status: 501, json: { error: { code: 'unexpected_combined_read', message: 'Unexpected document transport' } } });
    };
    const stale = () => route.fulfill({ status: 409,
      json: { error: { code: 'doc_visible_generation_stale', message: 'Combined selection expired' } } });
    if (route.request().method() !== 'GET') return reject();
    if (url.pathname === `${documentPath}/find`) {
      if (state.expired) return stale();
      const from = Number(parameters.get('from'));
      const to = Number(parameters.get('to'));
      const body: DocFindPage = { projection: legacy ? 'find-legacy-v1' : 'find-v1', hash: 'document',
        generation: legacy ? '' : state.generation, from, to, event_index: to, field: 0, byte_offset: 0, next_cursor: null,
        matches: [7, 8].map(index => ({ event_index: index, field: 0, byte_offset: 0, byte_length: 6, raw: false,
          preview: `needle in archived event ${index}`, preview_byte_offset: 0,
          reveal_cursor: legacy ? '' : `${state.generation}-reveal-${index}`, source_range: { from: fragmentBytes, to: fragmentBytes + 6 } })) };
      return route.fulfill({ json: body });
    }
    if (url.pathname === `${documentPath}/find-fragments`) {
      const selection = parameters.get('selection');
      if (state.expired || !legacy && !selection?.startsWith(`${state.generation}-reveal-`)) return stale();
      const index = legacy ? Number(parameters.get('index')) : Number(selection!.split('-').at(-1));
      const offset = Number(parameters.get('offset'));
      const text = `needle source event ${index} ${legacy ? 'legacy' : state.generation}`;
      const body: DocFindFragmentPage = { projection: legacy ? 'find-legacy-fragment-v1' : 'find-fragment-v1',
        hash: 'document', generation: legacy ? '' : state.generation, event_index: index, byte_offset: offset,
        total_bytes: fragmentBytes * 2, json_fragment: text, next: offset + new TextEncoder().encode(text).length, previous: 0 };
      return route.fulfill({ json: body });
    }
    if (url.pathname === `${documentPath}/event-fragments`) {
      if (parameters.get('index') !== '2') return reject();
      const offset = Number(parameters.get('offset'));
      return route.fulfill({ json: { event_index: 2, byte_offset: offset, total_bytes: fragmentTotalBytes,
        next: offset + fragmentBytes === fragmentTotalBytes ? -1 : offset + fragmentBytes, json_fragment: fragmentText(2, offset) } });
    }
    if (url.pathname !== `${documentPath}/events`) return reject();
    if (parameters.get('view') === 'raw') {
      if (parameters.get('offset') !== '1' || parameters.get('limit') !== '1') return reject();
      return route.fulfill({ json: { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' },
        total: pageCount * (legacy ? legacyPageSize : 620), inherited: 0,
        offset: 1, next: 2, events: [{ kind: 'tool_result', event_index: 1, output: rawResult(1) }] } });
    }
    if (legacy) {
      if (parameters.has('projection')) return route.fulfill({ status: 409,
        json: { error: { code: 'doc_visible_projection_unsupported', message: 'Explicit legacy negotiation' } } });
      return route.fulfill({ json: legacyPage(Math.max(0, Number(parameters.get('offset'))), pageCount) });
    }
    if (parameters.get('projection') !== 'visible-v1') return reject();
    const token = parameters.get('cursor') ?? parameters.get('expand');
    if (token && (state.expired || !token.startsWith(`${state.generation}-`))) return stale();
    if (parameters.get('view') === 'prompts') {
      const total = pageCount * 620;
      const body: DocVisiblePage = { ...visiblePage(0, pageCount, state.generation), view: 'prompts', from: 0, to: total,
        replay_cursor: `${state.generation}-prompts-0`, next_cursor: null,
        units: [{ from: 0, to: total, turn_index: -1, event: null,
          fold: { from: 0, to: total, rows: total, cursor: `${state.generation}-prompts-fold-0` } }] };
      return route.fulfill({ json: body });
    }
    if (parameters.has('expand')) {
      if (token !== `${state.generation}-fold-0`) return reject();
      return route.fulfill({ json: { ...memoryFold(0, pageCount), generation: state.generation,
        replay_cursor: `${state.generation}-fold-0` } });
    }
    if (token && !/^generation-(one|two)-main-\d+$/.test(token)) return reject();
    const from = token ? Number(token.split('-').at(-1)) : 0;
    return route.fulfill({ json: visiblePage(from, pageCount, state.generation) });
  });
  return { requests, unexpected, state };
}

async function visitPages(page: Page, pageCount: number, legacy = false, first = 1) {
  const scroll = page.getByTestId('document-scroll');
  for (let visited = first; visited <= pageCount; visited++) {
    const selector = legacy ? `[data-event-index="${visited * legacyPageSize - 1}"]` : `[data-visible-unit="${visited * 620 - 31}"]`;
    await expect.poll(async () => {
      const target = page.locator(selector);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator('.doc-events')).toHaveAttribute('aria-busy', 'false');
}

async function retention(page: Page) {
  return page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient;
    const queries = client.getQueryCache().getAll();
    const main = queries.find(query => query.queryKey[0] === 'doc-visible' && query.queryKey.length === 7 && query.getObserversCount() > 0);
    const records = (main?.state.data as { pages?: Record<string, unknown>[] } | undefined)?.pages ?? [];
    const bodies = queries.filter(query => query.meta?.documentBody && query.state.data !== undefined);
    return { records: records.length,
      metadataBodyLeaks: records.filter(record => ['events', 'units', 'initial'].some(field => field in record) ||
        Array.isArray(record.rows) && record.rows.some(row => ['blocks', 'input', 'output', 'redacted_summary'].some(field => field in row))).length,
      retainedBodies: bodies.length, bodyKinds: [...new Set(bodies.map(query => query.queryKey[8]))].sort(),
      serializedBytes: bodies.reduce((total, query) => total + new TextEncoder().encode(JSON.stringify(query.state.data)).length, 0),
      pendingReads: queries.filter(query => query.state.fetchStatus !== 'idle').length,
      firstBodyRetained: bodies.some(query => {
        const body = query.state.data as { kind?: string; from?: number; offset?: number };
        return body.kind === 'units' && body.from === 0 || body.kind === 'legacy' && body.offset === 0;
      }) };
  });
}

async function openDetails(page: Page, legacy = false) {
  if (!legacy) await page.locator('[data-visible-unit="0"] > button').click();
  const raw = page.locator('[data-event-index="1"]');
  if (!legacy) await raw.locator('[data-deferred-event="1"] > summary').click();
  await raw.locator('details:not([data-deferred-event]) > summary').click();
  await expect(raw.locator('pre')).toHaveText(rawResult(1));
  const detail = page.locator('[data-deferred-event="2"]');
  await detail.locator(':scope > summary').focus();
  await page.keyboard.press('Space');
  await detail.getByRole('button', { name: 'Next fragment', exact: true }).click();
  await expect(detail.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await detail.locator('pre').evaluate(element => { element.scrollTop = 200; });
  await expect.poll(() => detail.locator('pre').evaluate(element => element.scrollTop)).toBe(200);
  await detail.locator(':scope > summary').focus();
  return detail;
}

async function expectRestoredDetails(page: Page) {
  const raw = page.locator('[data-event-index="1"]');
  await expect(raw.locator('details:not([data-deferred-event])')).toHaveAttribute('open', '');
  await expect(raw.locator('pre')).toHaveText(rawResult(1));
  const detail = page.locator('[data-deferred-event="2"]');
  await expect(detail).toHaveAttribute('open', '');
  await expect(detail.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await expect(detail.locator('pre')).toHaveText(fragmentText(2, fragmentBytes));
  await expect.poll(() => detail.locator('pre').evaluate(element => element.scrollTop)).toBe(200);
}

async function searchAndReselect(page: Page, capture: Awaited<ReturnType<typeof transport>>, pageCount: number, legacy = false) {
  const scroll = page.getByTestId('document-scroll');
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  const position = await scroll.evaluate(element => element.scrollTop);
  const eventReads = capture.requests.filter(url => url.pathname.endsWith('/events')).length;
  await page.getByRole('textbox', { name: 'Find text', exact: true }).fill('needle');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.locator('.document-find-preview mark')).toHaveText('needle');
  const searches = capture.requests.filter(url => url.pathname.endsWith('/find'));
  expect(searches).toHaveLength(1);
  expect(Object.fromEntries(searches[0].searchParams)).toEqual({ q: 'needle', from: '0', to: String(pageCount * (legacy ? legacyPageSize : 620)),
    ...(legacy ? { legacy: '1', base: 'base', total: String(pageCount * legacyPageSize), inherited: '0' } : { selection: 'generation-one-main-0' }) });
  const reveal = page.getByRole('button', { name: 'Read the matching conversation', exact: true });
  await reveal.click();
  await expect(page.locator('.document-find-source')).toHaveText(`needle source event 7 ${legacy ? 'legacy' : 'generation-one'}`);
  await page.getByRole('button', { name: 'Next result', exact: true }).click();
  await expect(page.getByText('Match 2', { exact: true })).toBeVisible();
  await reveal.click();
  await expect(page.locator('.document-find-source')).toContainText('needle source event 8');
  await page.getByRole('button', { name: 'Previous result', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await reveal.click();
  await expect(page.locator('.document-find-source')).toContainText('needle source event 7');
  expect(capture.requests.filter(url => url.pathname.endsWith('/find'))).toHaveLength(1);
  expect(capture.requests.filter(url => url.pathname.endsWith('/find-fragments'))).toHaveLength(3);
  expect(capture.requests.filter(url => url.pathname.endsWith('/events'))).toHaveLength(eventReads);
  expect(await scroll.evaluate(element => element.scrollTop)).toBe(position);
  return reveal;
}

if (documentWindowEnabled) test.describe('document combined adoption candidate', () => {
for (const pageCount of [10, 100]) test(`combined visible adoption keeps detail, focus and search scoped after ${pageCount} pages and restart`, async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const capture = await transport(page, pageCount);
  const errors = await harness(page);
  const detail = await openDetails(page);
  const summary = detail.locator(':scope > summary');
  const focused = await summary.elementHandle();
  expect((await retention(page)).bodyKinds).toEqual(expect.arrayContaining(['units', 'fold', 'event', 'fragment']));
  await visitPages(page, 2, false, 2);
  await expect(summary).toBeFocused();
  await expect(page.locator('[data-document-page="units:0"]')).toHaveAttribute('data-page-mounted', 'true');
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(summary).toBeFocused();
  expect(await focused!.evaluate(element => element.isConnected && element === document.activeElement)).toBe(true);
  await page.getByRole('combobox', { name: 'View mode', exact: true }).focus();
  await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator('[data-document-page="units:0"]')).toHaveAttribute('data-page-mounted', 'false');
  await visitPages(page, pageCount, false, 3);
  await expect(detail).not.toBeAttached();
  await expect.poll(async () => (await retention(page)).pendingReads).toBe(0);
  await expect.poll(async () => (await retention(page)).retainedBodies).toBeLessThanOrEqual(DOCUMENT_BODY_CACHE_PAGES);
  const observation = await retention(page);
  expect(observation).toMatchObject({ records: pageCount, metadataBodyLeaks: 0, firstBodyRetained: false });
  expect(observation.serializedBytes).toBeLessThanOrEqual(DOCUMENT_BODY_CACHE_BYTES);
  expect(capture.requests.filter(url => url.searchParams.get('cursor')?.startsWith('generation-one-main-'))
    .map(url => url.searchParams.get('cursor'))).toEqual(Array.from({ length: pageCount - 1 }, (_unused, position) => `generation-one-main-${(position + 1) * 620}`));
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expectRestoredDetails(page);
  expect(capture.requests.filter(url => url.searchParams.get('cursor') === 'generation-one-main-0')).toHaveLength(1);
  expect(capture.requests.filter(url => url.pathname.endsWith('/event-fragments') && url.searchParams.get('offset') === '0')).toHaveLength(1);
  const reveal = await searchAndReselect(page, capture, pageCount);
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(page.locator('.document-find-source')).toContainText('needle source event 7 generation-one');
  await page.getByRole('textbox', { name: 'Find text', exact: true }).press('Escape');
  await expect(page.getByRole('button', { name: 'Find in document', exact: true })).toBeFocused();
  await expectRestoredDetails(page);
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.locator('[data-visible-unit="0"] > button')).toHaveAccessibleName(`${pageCount * 620} AI events`);
  await expect(page.locator('[data-deferred-event="2"], .document-find-panel')).toHaveCount(0);
  await page.getByLabel('View mode').selectOption('chat');
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(page.locator('[data-visible-unit="0"] > button')).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text', exact: true }).fill('needle');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await reveal.click();
  await expect(page.locator('.document-find-source')).toContainText('generation-one');
  capture.state.expired = true;
  await reveal.click();
  await reveal.click();
  await expect(page.locator('.document-find-reveal').getByRole('alert')).toContainText('Combined selection expired');
  await expect(page.locator('.document-find-source')).toHaveCount(0);
  const initialReads = () => capture.requests.filter(url => url.pathname.endsWith('/events') &&
    url.searchParams.get('view') === 'chat' && !url.searchParams.has('cursor') && !url.searchParams.has('expand'));
  expect(initialReads()).toHaveLength(1);
  capture.state.expired = false;
  capture.state.generation = 'generation-two';
  await page.getByRole('button', { name: 'Restart document', exact: true }).click();
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(page.locator('[data-visible-unit="0"]')).toContainText('generation-two: Anchor 30');
  await expect(page.locator('.document-find-panel, .document-find-source')).toHaveCount(0);
  await expect(page.locator('[data-visible-unit="0"] > button')).toHaveAttribute('aria-expanded', 'false');
  await page.locator('[data-visible-unit="0"] > button').click();
  await expect(page.locator('[data-deferred-event="1"]')).not.toHaveAttribute('open');
  await expect(page.locator('[data-deferred-event="2"]')).not.toHaveAttribute('open');
  expect(initialReads()).toHaveLength(2);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text', exact: true }).fill('needle');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  expect(capture.requests.filter(url => url.pathname.endsWith('/find')).at(-1)!.searchParams.get('selection')).toBe('generation-two-main-0');
  await reveal.click();
  await expect(page.locator('.document-find-source')).toHaveText('needle source event 7 generation-two');
  expect(capture.requests.filter(url => url.pathname.endsWith('/find-fragments')).at(-1)!.searchParams.get('selection')).toBe('generation-two-reveal-7');
  expect(capture.unexpected).toEqual([]);
  expect(errors).toEqual([]);
  await testInfo.attach('combined-visible-adoption', { contentType: 'application/json', body: JSON.stringify({ pageCount, ...observation }) });
});

test('combined legacy adoption restores ten page bodies and selected source without inventing a generation', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const capture = await transport(page, 10, true);
  const errors = await harness(page, 'all');
  const detail = await openDetails(page, true);
  const summary = detail.locator(':scope > summary');
  expect((await retention(page)).bodyKinds).toEqual(expect.arrayContaining(['legacy', 'fragment']));
  await visitPages(page, 2, true, 2);
  await expect(summary).toBeFocused();
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(summary).toBeFocused();
  await page.getByLabel('View mode').focus();
  await visitPages(page, 10, true, 3);
  await expect(detail).not.toBeAttached();
  await expect.poll(async () => (await retention(page)).pendingReads).toBe(0);
  await expect.poll(async () => (await retention(page)).retainedBodies).toBeLessThanOrEqual(DOCUMENT_BODY_CACHE_PAGES);
  const observation = await retention(page);
  expect(observation).toMatchObject({ records: 10, metadataBodyLeaks: 0, firstBodyRetained: false });
  expect(observation.serializedBytes).toBeLessThanOrEqual(DOCUMENT_BODY_CACHE_BYTES);
  await expect.poll(() => page.locator('[data-event-index]').count()).toBeLessThanOrEqual(60);
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expectRestoredDetails(page);
  const replay = capture.requests.filter(url => url.pathname.endsWith('/events') && !url.searchParams.has('projection') && url.searchParams.get('offset') === '0');
  expect(replay).toHaveLength(1);
  expect(Object.fromEntries(replay[0].searchParams)).toEqual({ view: 'all', offset: '0', limit: '20', base: 'base' });
  await searchAndReselect(page, capture, 10, true);
  const fragments = capture.requests.filter(url => url.pathname.endsWith('/find-fragments'));
  expect(fragments.map(url => url.searchParams.get('index'))).toEqual(['7', '8', '7']);
  expect(fragments.every(url => url.searchParams.get('legacy') === '1' && url.searchParams.get('total') === '200' &&
    !url.searchParams.has('selection') && !url.searchParams.has('generation'))).toBe(true);
  await page.getByRole('textbox', { name: 'Find text', exact: true }).press('Escape');
  await expectRestoredDetails(page);
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.locator('[data-legacy-turn="0"]')).toBeAttached();
  await expect(page.locator('[data-deferred-event="2"], .document-find-panel')).toHaveCount(0);
  const negotiated = capture.requests.filter(url => url.searchParams.has('projection'));
  expect(negotiated.map(url => url.searchParams.get('view'))).toEqual(['all', 'prompts']);
  expect(capture.requests.some(url => url.searchParams.has('cursor') || url.searchParams.has('expand') || url.searchParams.get('view') === 'raw')).toBe(false);
  expect(capture.unexpected).toEqual([]);
  expect(errors).toEqual([]);
  await testInfo.attach('combined-legacy-adoption', { contentType: 'application/json', body: JSON.stringify(observation) });
});

test('combined responsive adoption preserves pinned focus and restored details across pane/window/pane scrolling', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const capture = await transport(page, 10);
  const errors = await harness(page, 'chat', true);
  const scroll = page.getByTestId('document-scroll');
  await expect(scroll).toHaveCSS('overflow-y', 'auto');
  const detail = await openDetails(page);
  const summary = detail.locator(':scope > summary');
  const focused = await summary.elementHandle();
  const firstPage = page.locator('[data-document-page="units:0"]');
  await visitPages(page, 10, false, 2);
  await expect(summary).toBeFocused();
  await expect(firstPage).toHaveAttribute('data-page-mounted', 'true');
  await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBeGreaterThan(0);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(scroll).toHaveCSS('overflow-y', 'visible');
  await expect(summary).toBeFocused();
  expect(await focused!.evaluate(element => element.isConnected && element === document.activeElement)).toBe(true);
  const terminal = page.locator('[data-visible-unit="6169"]');
  await expect.poll(async () => {
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    if (!await terminal.count()) return false;
    return terminal.evaluate(element => {
      const bounds = element.getBoundingClientRect();
      return bounds.bottom > 0 && bounds.top < innerHeight;
    });
  }).toBe(true);
  await expect(terminal).toBeInViewport();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  await expect(summary).toBeFocused();
  const terminalToggle = terminal.locator(':scope > button');
  await terminalToggle.focus();
  await expect(terminalToggle).toBeFocused();
  await expect(firstPage).toHaveAttribute('data-page-mounted', 'false');
  await expect(detail).not.toBeAttached();
  expect(await focused!.evaluate(element => element.isConnected)).toBe(false);
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  await page.evaluate(() => window.scrollTo(0, 0));
  await expectRestoredDetails(page);
  await expect(terminalToggle).toBeFocused();
  await expect(firstPage).toHaveAttribute('data-page-mounted', 'true');
  await summary.focus();
  const restored = await summary.elementHandle();
  await page.setViewportSize({ width: 1280, height: 800 });
  await expect(scroll).toHaveCSS('overflow-y', 'auto');
  await expect(summary).toBeFocused();
  expect(await restored!.evaluate(element => element.isConnected && element === document.activeElement)).toBe(true);
  await summary.scrollIntoViewIfNeeded();
  await expect(summary).toBeInViewport();
  await expectRestoredDetails(page);
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  await expect.poll(async () => (await retention(page)).pendingReads).toBe(0);
  const observation = await retention(page);
  expect(observation).toMatchObject({ records: 10, metadataBodyLeaks: 0 });
  expect(capture.requests.filter(url => url.pathname.endsWith('/events') && url.searchParams.get('view') === 'chat' &&
    !url.searchParams.has('cursor') && !url.searchParams.has('expand'))).toHaveLength(1);
  expect(capture.requests.some(url => url.pathname.endsWith('/events') && !url.searchParams.has('projection') && url.searchParams.get('view') !== 'raw')).toBe(false);
  expect(capture.unexpected).toEqual([]);
  expect(errors).toEqual([]);
  await testInfo.attach('combined-responsive-adoption', { contentType: 'application/json', body: JSON.stringify({
    roots: ['pane', 'window', 'pane'], pages: 10, fragmentOffset: fragmentBytes, ...observation,
  }) });
});
});
