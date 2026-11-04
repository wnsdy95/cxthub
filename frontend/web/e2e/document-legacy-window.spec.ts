import { test, expect, type Page } from '@playwright/test';
import type { DocViewEventPage } from '../src/api';
import type { DocViewEvent } from '../src/types';

const documentWindowEnabled = process.env.CXT_DOCUMENT_WINDOW === '1' || process.env.CXT_DOCUMENT_BASELINE === '1';
const messagesPerPage = 20;
type Bootstrap = 'unsupported' | 'older-server';
type StubResponse = { status?: number; json: unknown };

async function harness(page: Page, mode = 'chat') {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/__legacy-window-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  await page.goto(`/__legacy-window-harness?mode=${mode}`);
  return errors;
}

async function legacyRoutes(page: Page, bootstrap: Bootstrap, respond: (url: URL) => StubResponse) {
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (bootstrap === 'unsupported' && url.searchParams.has('projection')) return route.fulfill({ status: 409,
      json: { error: { code: 'doc_visible_projection_unsupported', message: 'Legacy document' } } });
    return route.fulfill(respond(url));
  });
  return requests;
}

function message(index: number, text: string, role = 'assistant'): DocViewEvent {
  return { kind: 'message', event_index: index, seq: index + 9000, role, blocks: [{ type: 'text', text }] };
}

function legacyPage(offset: number, events: DocViewEvent[], total: number): DocViewEventPage {
  const end = offset + events.length;
  return { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' }, offset, total, inherited: 0,
    next: end === total ? -1 : end, events };
}

function longMessage(index: number, role = 'assistant') {
  return message(index, `Legacy body ${index}\n\n` + 'A readable paragraph keeps each legacy page independently scrollable.\n\n'.repeat(6), role);
}

function windowPage(url: URL, pageCount: number): DocViewEventPage {
  const offset = Math.max(0, Number(url.searchParams.get('offset')));
  return legacyPage(offset, Array.from({ length: messagesPerPage }, (_unused, position) => longMessage(offset + position)), pageCount * messagesPerPage);
}

function expectBootstrap(requests: URL[], bootstrap: Bootstrap, mode: string) {
  const visible = requests.filter(url => url.searchParams.has('projection'));
  expect(visible).toHaveLength(1);
  expect(Object.fromEntries(visible[0].searchParams)).toEqual({ projection: 'visible-v1', view: mode, offset: '-1', limit: '20', base: 'base' });
  const directInitial = requests.filter(url => !url.searchParams.has('projection') && url.searchParams.get('offset') === '-1');
  expect(directInitial).toHaveLength(bootstrap === 'unsupported' ? 1 : 0);
  if (directInitial.length) expect(Object.fromEntries(directInitial[0].searchParams)).toEqual({ view: mode, offset: '-1', limit: '20', base: 'base' });
  expect(requests.every(url => url.pathname.endsWith('/repos/repo/docs/document/events'))).toBe(true);
  expect(requests.some(url => url.searchParams.has('cursor') || url.searchParams.has('expand') || url.searchParams.get('view') === 'raw')).toBe(false);
}

async function visitPages(page: Page, pageCount: number, lastRow = messagesPerPage - 1) {
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 1; visited <= pageCount; visited++) {
    const index = (visited - 1) * messagesPerPage + lastRow;
    await expect.poll(async () => {
      const target = page.locator(`[data-event-index="${index}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator(`[data-event-index="${(pageCount - 1) * messagesPerPage + lastRow}"]`)).toBeVisible();
  await expect(page.locator('.doc-events')).toHaveAttribute('aria-busy', 'false');
}

async function cacheObservation(page: Page) {
  return page.evaluate(() => {
    type CachedRecord = { kind?: string; offset?: number; rows?: Record<string, unknown>[]; initial?: unknown; events?: unknown };
    type CachedQuery = { queryKey: readonly unknown[]; meta?: { documentBody?: boolean }; getObserversCount(): number;
      state: { status: string; fetchStatus: string; data?: { pages?: CachedRecord[]; events?: unknown }; error?: { message: string } | null } };
    const client = (window as unknown as { documentQueryClient: { getQueryCache(): { getAll(): CachedQuery[] } } }).documentQueryClient;
    const queries = client.getQueryCache().getAll();
    const main = queries.find(query => query.queryKey[0] === 'doc-visible' && query.queryKey.length === 7 && query.getObserversCount() > 0);
    const records = main?.state.data?.pages ?? [];
    const bodies = queries.filter(query => query.meta?.documentBody && query.state.data !== undefined);
    return {
      mainStatus: main?.state.status,
      mainFetchStatus: main?.state.fetchStatus,
      mainError: main?.state.error?.message ?? null,
      records: records.length,
      legacyRecords: records.filter(record => record.kind === 'legacy' && Array.isArray(record.rows)).length,
      metadataRows: records.reduce((total, record) => total + (record.rows?.length ?? 0), 0),
      metadataBodyLeaks: records.filter(record => 'events' in record || 'initial' in record || record.rows?.some(row =>
        ['blocks', 'input', 'output', 'redacted_summary'].some(field => field in row))).length,
      pinnedInitialBodies: queries.reduce((total, query) => total + (query.state.data?.pages?.filter(record => record.initial !== undefined).length ?? 0), 0),
      retainedBodies: bodies.length,
      activeBodies: bodies.filter(query => query.getObserversCount() > 0).length,
      firstBodyRetained: bodies.some(query => query.queryKey.at(-1) === 0),
      sharedBodiesOnly: queries.filter(query => query.queryKey.includes('body') || query.state.data?.events !== undefined)
        .every(query => query.meta?.documentBody === true && query.queryKey.includes('body')),
      pendingReads: queries.filter(query => query.state.fetchStatus !== 'idle').length,
      mountedRows: document.querySelectorAll('[data-event-index]').length,
    };
  });
}

for (const bootstrap of ['unsupported', 'older-server'] as const) {
  test(`legacy chat keeps one turn and cross-page AI work together (${bootstrap})`, async ({ page }) => {
    const first = legacyPage(0, [
      message(0, 'Human prompt across the page boundary\n\n' + 'Keep reading this prompt before loading its answer.\n\n'.repeat(24), 'user'),
      { kind: 'tool_call', event_index: 1, tool_name: 'Bash', input: { command: 'printf legacy-boundary' } },
      { kind: 'reasoning', event_index: 2, redacted_summary: 'Nonempty reasoning belongs to this answer' },
      { kind: 'reasoning', event_index: 3, redacted_summary: '   ' },
    ], 6);
    const second = legacyPage(4, [
      { kind: 'tool_result', event_index: 4, output: 'Cross-page tool output\nPreserved detail' },
      message(5, 'One assistant answer after the boundary'),
    ], 6);
    const requests = await legacyRoutes(page, bootstrap, url => ({ json: Number(url.searchParams.get('offset')) > 0 ? second : first }));
    const errors = await harness(page);
    const turn = page.locator('[data-legacy-turn="0"]');
    await expect(turn.locator('[data-legacy-response="1"] > button')).toContainText('AI 2');
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
    const response = turn.locator('[data-legacy-response="1"]');
    await expect(response.locator('[data-event-index="5"]')).toBeVisible();
    await expect(page.locator('[data-legacy-turn]')).toHaveCount(1);
    await expect(page.locator('[data-legacy-response]')).toHaveCount(1);
    const toggle = response.locator(':scope > button');
    await expect(toggle).toContainText('AI 3');
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(response.locator('.asst-work [data-event-index]')).toHaveCount(0);
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(response.locator('.asst-work [data-event-index]')).toHaveCount(3);
    for (const index of [1, 2, 4]) await expect(response.locator(`[data-event-index="${index}"]`)).toBeAttached();
    await expect(page.locator('[data-event-index="3"]')).toHaveCount(0);
    await expect(response.locator('[data-event-index="4"] summary')).toContainText('Cross-page tool output');
    await toggle.click();
    await expect(response.locator('.asst-work')).toHaveCount(0);
    await toggle.click();
    await expect(response.locator('.asst-work [data-event-index]')).toHaveCount(3);
    expectBootstrap(requests, bootstrap, 'chat');
    expect(requests).toHaveLength(bootstrap === 'unsupported' ? 3 : 2);
    expect(Object.fromEntries(requests.at(-1)!.searchParams)).toEqual({ view: 'chat', offset: '4', limit: '20' });
    expect(errors).toEqual([]);
  });

  test(`legacy prompts expand leading work and exclude compact, seed and empty reasoning from prompt boundaries (${bootstrap})`, async ({ page }) => {
    const events: DocViewEvent[] = [
      { kind: 'tool_call', event_index: 0, tool_name: 'Bash', input: { command: 'printf leading-work' } },
      { kind: 'reasoning', event_index: 1, redacted_summary: '' },
      { ...message(2, 'Compacted leading history is not a prompt', 'user'), compact_summary: true },
      message(3, '[cxt seed] Branch-switch context: leading seed is not a prompt', 'user'),
      { kind: 'tool_result', event_index: 4, output: 'Leading tool output' },
      message(5, 'Actual human prompt\n\n' + 'A long prompt keeps the next legacy page offscreen.\n\n'.repeat(24), 'user'),
      { kind: 'reasoning', event_index: 6, redacted_summary: 'Reasoning before the boundary' },
      { kind: 'tool_call', event_index: 7, tool_name: 'Bash', input: { command: 'printf prompt-work' } },
      { kind: 'tool_result', event_index: 8, output: 'Prompt tool output after the boundary' },
      message(9, 'Assistant answer for the actual prompt'),
      { ...message(10, 'Compacted answer context is not a prompt', 'user'), compact_summary: true },
      message(11, '[cxt] This session was resumed from a branch context seed. Not a prompt', 'user'),
      { kind: 'reasoning', event_index: 12, redacted_summary: '\n ' },
      message(13, 'Next actual human prompt', 'user'),
      message(14, 'Answer for the next human prompt'),
    ];
    const requests = await legacyRoutes(page, bootstrap, url => {
      const offset = Math.max(0, Number(url.searchParams.get('offset')));
      return { json: legacyPage(offset, events.slice(offset, offset === 0 ? 8 : events.length), events.length) };
    });
    const errors = await harness(page, 'prompts');
    const leading = page.locator('[data-legacy-turn="-1"]');
    await expect(leading.locator(':scope > button')).toContainText('4 events before the first prompt');
    await expect(leading.locator('[data-event-index]')).toHaveCount(0);
    await leading.locator(':scope > button').click();
    for (const index of [0, 2, 3, 4]) await expect(leading.locator(`[data-event-index="${index}"]`)).toBeAttached();
    await expect(leading.locator('[data-event-index="1"]')).toHaveCount(0);
    await expect(leading.locator('[data-event-index="2"] .compact-summary summary')).toContainText('Context compaction summary');
    await expect(leading.locator('[data-event-index="3"] .compact-summary summary')).toContainText('cxt seed summary');
    await leading.locator(':scope > button').click();
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
    await expect(page.locator('[data-event-index="13"]')).toBeVisible();
    await expect(page.locator('[data-legacy-turn]')).toHaveCount(3);
    for (const index of [2, 3, 10, 11]) await expect(page.locator(`[data-legacy-turn="${index}"]`)).toHaveCount(0);
    const prompt = page.locator('[data-legacy-turn="5"]');
    const toggle = prompt.locator(':scope > .turn-head');
    await expect(toggle.locator('.turn-count')).toContainText('AI 6');
    await expect(prompt.locator('[data-event-index="9"]')).toHaveCount(0);
    await toggle.click();
    for (const index of [6, 7, 8, 9, 10, 11]) await expect(prompt.locator(`[data-event-index="${index}"]`)).toBeAttached();
    await expect(prompt.locator('[data-event-index="12"]')).toHaveCount(0);
    await toggle.click();
    await expect(prompt.locator('[data-event-index="9"]')).toHaveCount(0);
    await toggle.click();
    await expect(prompt.locator('[data-event-index="9"]')).toBeAttached();
    expectBootstrap(requests, bootstrap, 'prompts');
    expect(requests).toHaveLength(bootstrap === 'unsupported' ? 3 : 2);
    expect(errors).toEqual([]);
  });
}

test('legacy all view limits empty reasoning progress without observing invisible bodies', async ({ page }) => {
  const requests = await legacyRoutes(page, 'older-server', url => {
    const offset = Math.max(0, Number(url.searchParams.get('offset')));
    return { json: legacyPage(offset, Array.from({ length: 20 }, (_, position) => ({ kind: 'reasoning', event_index: offset + position,
      redacted_summary: '   ', locked: { provider: 'codex', scheme: 'encrypted', blob: 'private encrypted reasoning' } })), 200) };
  });
  const errors = await harness(page, 'all');
  const more = page.locator('.doc-load-trigger').getByRole('button', { name: 'Load more events', exact: true });
  await expect(more).toBeVisible();
  expect(requests).toHaveLength(2);
  await expect(page.locator('[data-event-index], [data-document-page]')).toHaveCount(0);
  expect(await cacheObservation(page)).toMatchObject({ records: 2, activeBodies: 0, mountedRows: 0, pendingReads: 0, metadataBodyLeaks: 0 });
  await more.click();
  await expect.poll(() => requests.length).toBe(4);
  await expect(more).toBeVisible();
  await expect(page.locator('[data-event-index], [data-document-page]')).toHaveCount(0);
  expect(await cacheObservation(page)).toMatchObject({ records: 4, activeBodies: 0, mountedRows: 0, pendingReads: 0 });
  expect(errors).toEqual([]);
});

test('legacy pending work preserves its focused disclosure when the response arrives', async ({ page }) => {
  let release: () => void = () => {};
  const responseReady = new Promise<void>(resolve => { release = resolve; });
  const requests: URL[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.searchParams.get('offset') === '2') {
      await responseReady;
      return route.fulfill({ json: legacyPage(2, [message(2, 'Response after focused work')], 3) });
    }
    return route.fulfill({ json: legacyPage(0, [longMessage(0, 'user'),
      { kind: 'tool_result', event_index: 1, output: 'Work result\nFocused detail must survive' }], 3) });
  });
  try {
    const errors = await harness(page);
    const response = page.locator('[data-legacy-response="1"]');
    await response.locator(':scope > button').focus();
    await page.keyboard.press('Space');
    const detail = response.locator('[data-event-index="1"] details');
    const summary = detail.locator('summary');
    await page.keyboard.press('Tab');
    await expect(summary).toBeFocused();
    await page.keyboard.press('Space');
    await summary.evaluate(element => Object.assign(window, { legacyFocusedSummary: element }));
    await expect(detail).toHaveAttribute('open', '');
    await page.locator('.doc-load-trigger').scrollIntoViewIfNeeded();
    await expect.poll(() => requests.length).toBe(2);
    release();
    await expect(response.locator('[data-event-index="2"]')).toBeAttached();
    await expect(detail).toHaveAttribute('open', '');
    await expect(summary).toBeFocused();
    expect(await summary.evaluate(element => (window as unknown as { legacyFocusedSummary: HTMLElement }).legacyFocusedSummary === element)).toBe(true);
    const geometry = await response.evaluate(element => {
      const work = element.querySelector('.asst-work')!.getBoundingClientRect();
      const anchor = element.querySelector('[data-event-index="2"]')!.getBoundingClientRect();
      return { workBottom: work.bottom, responseTop: anchor.top };
    });
    expect(geometry.workBottom).toBeLessThanOrEqual(geometry.responseTop);
    expect(errors).toEqual([]);
  } finally { release(); }
});

test.describe('legacy document window candidate', () => {
  test.skip(!documentWindowEnabled, 'Window-only cache and replay assertions');

  for (const mode of ['chat', 'prompts']) test(`one hundred legacy pages keep ${mode} wrappers and bodies bounded`, async ({ page }) => {
    test.setTimeout(180_000);
    const requests = await legacyRoutes(page, 'older-server', url => {
      const body = windowPage(url, 100);
      if (mode === 'prompts') body.events = body.events.map(event => ({ ...event, role: 'user' }));
      return { json: body };
    });
    const errors = await harness(page, mode);
    await visitPages(page, 100);
    await expect.poll(async () => (await cacheObservation(page)).retainedBodies).toBeLessThanOrEqual(8);
    const observation = await cacheObservation(page);
    expect(observation).toMatchObject({ records: 100, metadataRows: 2000, metadataBodyLeaks: 0, pinnedInitialBodies: 0, pendingReads: 0 });
    expect(observation.mountedRows).toBeLessThanOrEqual(60);
    expect(await page.locator(mode === 'chat' ? '[data-legacy-response]' : '[data-legacy-turn]').count()).toBeLessThanOrEqual(60);
    expectBootstrap(requests, 'older-server', mode);
    expect(requests).toHaveLength(100);
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
    await expect(page.locator('[data-event-index="0"]')).toBeVisible();
    expect(requests.filter(url => url.searchParams.get('offset') === '0')).toHaveLength(1);
    expect(errors).toEqual([]);
  });

  for (const pageCount of [10, 100]) test(`bounds all ${pageCount} legacy 20-message page bodies and replays the first directly`, async ({ page }, testInfo) => {
    test.setTimeout(180_000);
    const bootstrap = pageCount === 10 ? 'unsupported' : 'older-server';
    const requests = await legacyRoutes(page, bootstrap, url => ({ json: windowPage(url, pageCount) }));
    const errors = await harness(page, 'all');
    await visitPages(page, pageCount);
    await expect(page.locator('[data-document-page="units:0"]')).toHaveAttribute('data-page-mounted', 'false');
    await expect.poll(async () => (await cacheObservation(page)).pendingReads).toBe(0);
    await expect.poll(async () => (await cacheObservation(page)).retainedBodies).toBeLessThanOrEqual(8);
    const observation = await cacheObservation(page);
    expect(observation).toMatchObject({ mainStatus: 'success', mainFetchStatus: 'idle', mainError: null, records: pageCount,
      legacyRecords: pageCount, metadataRows: pageCount * messagesPerPage, metadataBodyLeaks: 0, pinnedInitialBodies: 0,
      firstBodyRetained: false, sharedBodiesOnly: true });
    expect(observation.retainedBodies).toBeGreaterThan(0);
    expect(observation.activeBodies).toBeLessThanOrEqual(3);
    expect(observation.mountedRows).toBeLessThanOrEqual(60);
    expectBootstrap(requests, bootstrap, 'all');
    expect(requests).toHaveLength(pageCount + (bootstrap === 'unsupported' ? 1 : 0));
    const continuations = requests.filter(url => !url.searchParams.has('projection') && Number(url.searchParams.get('offset')) > 0);
    expect(continuations.map(url => Number(url.searchParams.get('offset')))).toEqual(Array.from({ length: pageCount - 1 }, (_unused, position) => (position + 1) * messagesPerPage));
    expect(continuations.every(url => !url.searchParams.has('base') && url.searchParams.get('view') === 'all')).toBe(true);
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
    await expect(page.locator('[data-event-index="0"]')).toBeVisible();
    const firstReplay = requests.filter(url => !url.searchParams.has('projection') && url.searchParams.get('offset') === '0');
    expect(firstReplay).toHaveLength(1);
    expect(Object.fromEntries(firstReplay[0].searchParams)).toEqual({ view: 'all', offset: '0', limit: '20', base: 'base' });
    expectBootstrap(requests, bootstrap, 'all');
    await testInfo.attach('legacy-document-window-retention', { contentType: 'application/json',
      body: JSON.stringify({ visited: pageCount, messagesPerPage, bootstrap, firstReplayRequests: firstReplay.length, ...observation }, null, 2) });
    expect(errors).toEqual([]);
  });

  for (const failure of ['permission', 'stale', 'malformed'] as const) test(`legacy replay ${failure} stays local and requires an explicit user action`, async ({ page }) => {
    test.setTimeout(90_000);
    let recovered = false;
    const requests = await legacyRoutes(page, 'older-server', url => {
      const body = windowPage(url, 12);
      if (url.searchParams.get('offset') !== '20' || recovered || requests.filter(request => !request.searchParams.has('projection') && request.searchParams.get('offset') === '20').length < 2) return { json: body };
      if (failure === 'permission') return { status: 403, json: { error: { code: 'forbidden', message: 'Legacy replay access denied' } } };
      if (failure === 'stale') return { status: 409, json: { error: { code: 'doc_visible_generation_stale', message: 'Legacy source changed' } } };
      return { json: { ...body, kind: 'legacy' } };
    });
    const errors = await harness(page, 'all');
    await visitPages(page, 12);
    const recycled = page.locator('[data-document-page="units:20"]');
    await expect(recycled).toHaveAttribute('data-page-mounted', 'false');
    await recycled.evaluate(element => element.scrollIntoView({ block: 'start' }));
    const alert = recycled.getByRole('alert');
    await expect(alert).toContainText(failure === 'permission' ? 'Legacy replay access denied' : failure === 'stale'
      ? 'Legacy source changed' : 'Invalid or changed document projection');
    const action = failure === 'permission' ? 'Retry' : 'Restart document';
    await expect(recycled.getByRole('button', { name: action, exact: true })).toBeVisible();
    await expect(recycled.locator('[data-event-index]')).toHaveCount(0);
    expect(await cacheObservation(page)).toMatchObject({ mainStatus: 'success', mainFetchStatus: 'idle', mainError: null,
      records: 12, legacyRecords: 12, metadataRows: 240, metadataBodyLeaks: 0, pinnedInitialBodies: 0 });
    expectBootstrap(requests, 'older-server', 'all');
    const readsBeforeRecycling = requests.length;
    await page.getByRole('combobox', { name: 'View mode' }).focus();
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
    await expect(recycled).toHaveAttribute('data-page-mounted', 'false');
    await page.clock.install();
    await page.clock.fastForward(65_000);
    await page.clock.resume();
    await recycled.evaluate(element => element.scrollIntoView({ block: 'start' }));
    await expect(alert).toBeVisible();
    await page.waitForTimeout(250);
    expect(requests).toHaveLength(readsBeforeRecycling);
    expect(requests.filter(url => url.searchParams.get('offset') === '20')).toHaveLength(2);
    expectBootstrap(requests, 'older-server', 'all');
    recovered = true;
    await recycled.getByRole('button', { name: action, exact: true }).click();
    if (failure === 'permission') {
      await expect(recycled.locator('[data-event-index="20"]')).toBeVisible();
      expect(requests.filter(url => url.searchParams.get('offset') === '20')).toHaveLength(3);
      expectBootstrap(requests, 'older-server', 'all');
      expect(Object.fromEntries(requests.at(-1)!.searchParams)).toEqual({ view: 'all', offset: '20', limit: '20' });
    } else {
      await expect.poll(() => requests.filter(url => url.searchParams.has('projection')).length).toBe(2);
      expect(Object.fromEntries(requests.filter(url => url.searchParams.has('projection')).at(-1)!.searchParams))
        .toEqual({ projection: 'visible-v1', view: 'all', offset: '-1', limit: '20', base: 'base' });
      await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
      await expect(page.locator('[data-event-index="0"]')).toBeVisible();
    }
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(errors).toEqual([]);
  });

  test('legacy prompt expansion and nested disclosure states survive collapse and window recycling', async ({ page }) => {
    test.setTimeout(90_000);
    const requests = await legacyRoutes(page, 'older-server', url => {
      const body = windowPage(url, 12);
      body.events = body.events.map(event => event.event_index! % 2 === 0 ? { ...event, role: 'user' } : event);
      if (body.offset === 0) {
        body.events[0] = message(0, 'Expand this prompt and preserve its work', 'user');
        body.events[1] = { kind: 'tool_result', event_index: 1, output: 'Preserved legacy result\nSecond result line' };
        body.events[2] = { kind: 'reasoning', event_index: 2, redacted_summary: 'This reasoning remains collapsed' };
      }
      return { json: body };
    });
    const errors = await harness(page, 'prompts');
    const turn = page.locator('[data-legacy-turn="0"]');
    const toggle = turn.locator(':scope > .turn-head');
    await expect(toggle.locator('.turn-count')).toContainText('AI 3');
    await toggle.click();
    const result = turn.locator('[data-event-index="1"] details');
    const reasoning = turn.locator('[data-event-index="2"] details');
    await result.locator('summary').click();
    await expect(result).toHaveAttribute('open', '');
    await expect(reasoning).not.toHaveAttribute('open', '');
    await toggle.click();
    await expect(result).not.toBeAttached();
    await toggle.click();
    await expect(result).toHaveAttribute('open', '');
    await page.getByRole('combobox', { name: 'View mode' }).focus();
    await visitPages(page, 12, 18);
    await expect(turn).not.toBeAttached();
    await expect.poll(async () => (await cacheObservation(page)).firstBodyRetained).toBe(false);
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(result).toHaveAttribute('open', '');
    await expect(result.locator('pre')).toContainText('Second result line');
    await expect(reasoning).not.toHaveAttribute('open', '');
    await toggle.click();
    await expect(result).not.toBeAttached();
    await toggle.click();
    await expect(result).toHaveAttribute('open', '');
    expectBootstrap(requests, 'older-server', 'prompts');
    const firstReplay = requests.filter(url => url.searchParams.get('offset') === '0');
    expect(firstReplay).toHaveLength(1);
    expect(Object.fromEntries(firstReplay[0].searchParams)).toEqual({ view: 'prompts', offset: '0', limit: '20', base: 'base' });
    expect(errors).toEqual([]);
  });
});
