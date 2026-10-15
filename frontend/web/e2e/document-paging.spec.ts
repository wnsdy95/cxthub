import { test, expect, type Page } from '@playwright/test';
import type { DocViewEvent, PendingView } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture, wireFixture } from '../tests/pendingViewFixture';

const envelope = { cir_version: '2', source_provider: 'codex' };
const message = (index: number, text: string, role = 'user'): DocViewEvent => ({
  kind: 'message', role: role as 'user' | 'assistant', seq: index + 700, event_index: index, deferred: false,
  blocks: [{ type: 'text', text }],
});
const tool = (index: number, bytes = 800): DocViewEvent => ({
  kind: 'tool_call', seq: index + 700, event_index: index, deferred: true, event_bytes: bytes, tool_name: 'Bash',
});
function eventPage(events: DocViewEvent[], offset = 0, total = offset + events.length, inherited = 0, hash = 'document') {
  const end = offset + events.length;
  return { hash, envelope, events, offset, total, inherited, next: end < total ? end : -1 };
}

async function harness(page: Page, query = '') {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => {
    localStorage.setItem('cxt.locale', 'en');
    const sources: EventTarget[] = [];
    class Source extends EventTarget {
      constructor() { super(); sources.push(this); }
      close() { sources.splice(sources.indexOf(this), 1); }
    }
    Object.assign(window, { EventSource: Source, emitRepositoryRevision: (revision: unknown) => {
      for (const source of sources) source.dispatchEvent(new MessageEvent('revision', { data: JSON.stringify(revision) }));
    } });
  });
  await page.route('**/__document-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  await page.goto(`/__document-harness${query}`);
  return errors;
}

test('chat and prompts retain absolute turns while details use only exact raw ranges', async ({ page }) => {
  const requests: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url.search);
    const mode = url.searchParams.get('view');
    const index = Number(url.searchParams.get('offset'));
    const events = mode === 'raw' ? [{ ...message(index, `Detail at ${index}`, 'assistant'), deferred: false }]
      : [message(99, 'Inherited prompt'), tool(100), mode === 'prompts'
        ? { kind: 'message' as const, role: 'assistant' as const, seq: 999, event_index: 101, deferred: true }
        : message(101, 'Visible answer', 'assistant')];
    await route.fulfill({ json: eventPage(events, mode === 'raw' ? index : 99, 102, 100) });
  });
  const errors = await harness(page, '?start=99');
  await expect(page.getByText('Visible answer', { exact: true })).toBeVisible();
  expect(requests).toHaveLength(1);
  await expect(page.locator('[data-event-index="99"]')).toHaveClass('doc-inherited');
  await page.locator('.turn-head').click();
  expect(requests).toHaveLength(1);
  await page.locator('[data-deferred-event="100"] summary').click();
  await expect(page.getByText('Detail at 100', { exact: true })).toBeVisible();
  expect(requests[1]).toBe('?view=raw&offset=100&limit=1');
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.getByText('Visible answer', { exact: true })).toHaveCount(0);
  await expect(page.locator('.turn-head')).toHaveAttribute('aria-expanded', 'false');
  await page.locator('.turn-head').click();
  await expect(page.getByText('Detail at 101', { exact: true })).toBeVisible();
  expect(requests.some(query => query.includes('view=prompts') && query.includes('limit=20'))).toBe(true);
  expect(requests.filter(query => query.includes('view=raw'))).toEqual(['?view=raw&offset=100&limit=1', '?view=raw&offset=101&limit=1']);
  expect(errors).toEqual([]);
});

test('near-bottom prefetch stays hidden until the bottom and continuation waits for the last screenful', async ({ page }) => {
  const requests: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const hash = url.pathname.includes('/tail/') ? 'tail' : 'document';
    const offset = Math.max(0, Number(url.searchParams.get('offset')));
    requests.push(`${hash}:${url.searchParams.get('offset')}`);
    const events = hash === 'tail' ? [message(0, 'Continuation loaded')]
      : Array.from({ length: 20 }, (_, index) => message(offset + index, `Message ${offset + index}\n\n` + 'A visible paragraph.\n\n'.repeat(3)));
    await route.fulfill({ json: eventPage(events, offset, hash === 'tail' ? 1 : 40, 0, hash) });
  });
  const errors = await harness(page, '?tail');
  await expect(page.getByText('Message 19', { exact: false })).toBeAttached();
  expect(requests).toEqual(['document:-1']);
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight - element.clientHeight - 200; });
  await expect.poll(() => requests.length).toBe(2);
  await expect(page.locator('[data-event-index="20"]')).toHaveCount(0);
  await expect(page.getByTestId('continuation')).toHaveCount(0);
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator('[data-event-index="39"]')).toBeAttached();
  expect(requests).toEqual(['document:-1', 'document:20']);
  await expect(page.getByTestId('continuation')).toHaveCount(0);
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.getByText('Continuation loaded', { exact: true })).toBeVisible();
  expect(requests).toEqual(['document:-1', 'document:20', 'tail:-1']);
  expect(errors).toEqual([]);
});

test('root view HTTP metadata stays compact with a large envelope, tool-heavy prefetch and mode reset', async ({ page }) => {
  const requests: { mode: string | null; offset: number }[] = [];
  const bodies: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const mode = url.searchParams.get('view');
    const offset = Math.max(0, Number(url.searchParams.get('offset')));
    requests.push({ mode, offset: Number(url.searchParams.get('offset')) });
    const events = Array.from({ length: 20 }, (_, visible) => {
      const index = offset + visible * 21;
      return [message(index, `Screenful ${index}\n\n` + 'Readable response.\n\n'.repeat(4)),
        ...Array.from({ length: 20 }, (_, hidden) => ({ ...tool(index + hidden + 1, 512 * 1024 * 1024), provider_tool_name: 'functions.exec' }))];
    }).flat();
    const body = JSON.stringify({ ...eventPage(events, offset, 840), envelope: { ...envelope, source_models: ['m'.repeat(255 * 1024)] } });
    bodies.push(body);
    await route.fulfill({ contentType: 'application/json', body });
  });
  const errors = await harness(page);
  await expect(page.getByText('Screenful 399', { exact: true })).toBeAttached();
  expect(requests).toEqual([{ mode: 'chat', offset: -1 }]);
  expect(Buffer.byteLength(bodies[0])).toBeGreaterThan(256 * 1024);
  expect(Buffer.byteLength(bodies[0])).toBeLessThan(1024 * 1024);
  const body = JSON.parse(bodies[0]);
  expect(body.events).toHaveLength(420);
  expect(body.events.filter((event: DocViewEvent) => event.deferred)).toHaveLength(400);
  expect(body.events[1]).toMatchObject({ event_index: 1, event_bytes: 512 * 1024 * 1024, deferred: true, tool_name: 'Bash', provider_tool_name: 'functions.exec' });
  expect(body.events[1]).not.toHaveProperty('input');
  expect(body.events[1]).not.toHaveProperty('blocks');
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight - element.clientHeight - 200; });
  await expect.poll(() => requests.length).toBe(2);
  await expect(page.locator('[data-event-index="420"]')).toHaveCount(0);
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator('[data-event-index="819"]')).toBeAttached();
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.locator('[data-event-index="399"]')).toBeAttached();
  await expect(page.locator('[data-event-index="420"]')).toHaveCount(0);
  expect(requests).toEqual([{ mode: 'chat', offset: -1 }, { mode: 'chat', offset: 420 }, { mode: 'prompts', offset: -1 }]);
  expect(errors).toEqual([]);
});

test('hidden placeholder pages preserve turns and a failed next page retries only on request', async ({ page }) => {
  let fail = true;
  const reads: number[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const offset = Number(url.searchParams.get('offset'));
    reads.push(offset);
    if (offset === 512 && fail) {
      await route.fulfill({ status: 503, json: { error: { message: 'Page unavailable' } } });
      return;
    }
    await route.fulfill({ json: offset === -1
      ? eventPage([message(0, 'Retained prompt'), ...Array.from({ length: 511 }, (_, index) => tool(index + 1))], 0, 513)
      : eventPage([message(512, 'After hidden tools', 'assistant')], 512, 513) });
  });
  const errors = await harness(page);
  await expect(page.getByRole('alert')).toContainText('Page unavailable');
  await expect(page.getByText('Retained prompt', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: '▸ AI 511', exact: true }).click();
  await expect(page.locator('[data-deferred-event]')).toHaveCount(511);
  await page.waitForTimeout(350);
  expect(reads).toEqual([-1, 512]);
  fail = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.getByText('After hidden tools', { exact: true })).toBeAttached();
  await expect(page.locator('[data-deferred-event]')).toHaveCount(511);
  expect(reads).toEqual([-1, 512, 512]);
  expect(errors).toEqual([]);
});

test('expanded tool details survive an assistant response appended across a page boundary', async ({ page }) => {
  let releaseNext!: () => void;
  const nextPage = new Promise<void>(resolve => { releaseNext = resolve; });
  let details = 0;
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const offset = Number(url.searchParams.get('offset'));
    if (url.searchParams.get('view') === 'raw') {
      details++;
      await route.fulfill({ json: eventPage([{ ...tool(1), deferred: false, input: { command: 'kept across pages' } }], 1, 3) });
    } else if (offset === 2) {
      await nextPage;
      await route.fulfill({ json: eventPage([message(2, 'Final response', 'assistant')], 2, 3) });
    } else await route.fulfill({ json: eventPage([message(0, 'Prompt before tools'), tool(1)], 0, 3) });
  });
  const errors = await harness(page);
  try {
    await page.getByRole('button', { name: '▸ AI 1', exact: true }).click();
    await page.locator('[data-deferred-event="1"] > summary').click();
    await expect(page.locator('.tool-cmd')).toHaveText('$ kept across pages');
    releaseNext();
    await expect(page.getByText('Final response', { exact: true })).toBeVisible();
    await expect(page.locator('[data-deferred-event="1"]')).toHaveAttribute('open', '');
    await expect(page.locator('.tool-cmd')).toHaveText('$ kept across pages');
    expect(details).toBe(1);
    expect(errors).toEqual([]);
  } finally { releaseNext(); }
});

test('hidden-only runs stop automatic scanning after two pages until explicitly continued', async ({ page }) => {
  const reads: number[] = [];
  await page.route('**/api/v1/**', async route => {
    const offset = Math.max(0, Number(new URL(route.request().url()).searchParams.get('offset')));
    reads.push(offset);
    const events = offset < 1024 ? Array.from({ length: 512 }, (_, index) => tool(offset + index))
      : [message(1024, 'Reached visible conversation')];
    await route.fulfill({ json: eventPage(events, offset, 1025) });
  });
  const errors = await harness(page);
  await expect(page.getByRole('button', { name: 'Load more events', exact: true })).toBeVisible();
  await page.waitForTimeout(300);
  expect(reads).toEqual([0, 512]);
  await page.getByRole('button', { name: 'Load more events', exact: true }).click();
  await expect(page.getByText('Reached visible conversation', { exact: true })).toBeVisible();
  expect(reads).toEqual([0, 512, 1024]);
  await expect(page.getByRole('button', { name: 'Load more events', exact: true })).toHaveCount(0);
  expect(errors).toEqual([]);
});

test('cached hidden pages obey the scan limit after returning to a mode', async ({ page }) => {
  const reads: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const offset = Math.max(0, Number(url.searchParams.get('offset')));
    reads.push(`${url.searchParams.get('view')}:${offset}`);
    await route.fulfill({ json: eventPage(Array.from({ length: 512 }, (_, index) => tool(offset + index)), offset, 2560) });
  });
  const errors = await harness(page);
  const more = page.getByRole('button', { name: 'Load more events', exact: true });
  await expect(more).toBeVisible();
  await expect(page.getByRole('button', { name: '\u25b8 AI 1024', exact: true })).toBeVisible();
  await more.click();
  await expect(page.getByRole('button', { name: '\u25b8 AI 2048', exact: true })).toBeVisible();
  await expect(more).toBeVisible();
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(more).toBeVisible();
  await page.getByLabel('View mode').selectOption('chat');
  await expect(more).toBeVisible();
  await expect(page.getByRole('button', { name: '\u25b8 AI 1024', exact: true })).toBeVisible();
  await more.click();
  await expect(page.getByRole('button', { name: '\u25b8 AI 2048', exact: true })).toBeVisible();
  expect(reads.filter(read => read.startsWith('chat:'))).toEqual(['chat:0', 'chat:512', 'chat:1024', 'chat:1536']);
  expect(errors).toEqual([]);
});

test('a deferred 512 MiB event loads one 64 KiB JSON fragment at a time without retaining prior chunks', async ({ page }) => {
  const bytes = 512 * 1024 * 1024;
  const requests: string[] = [];
  let fragmentFails = true;
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url.pathname + url.search);
    if (url.pathname.endsWith('/event-fragments')) {
      const offset = Number(url.searchParams.get('offset'));
      if (offset === 65536 && fragmentFails) {
        await route.fulfill({ status: 503, json: { error: { message: 'Fragment unavailable' } } });
        return;
      }
      await route.fulfill({ json: { event_index: 7, byte_offset: offset, next: offset + 65536, total_bytes: bytes,
        json_fragment: (offset === 0 ? 'FIRST' : 'SECOND').padEnd(65536, 'x') } });
      return;
    }
    await route.fulfill({ json: eventPage([tool(7, bytes)], 7, 8) });
  });
  const errors = await harness(page, '?mode=all&start=7');
  await expect(page.locator('[data-deferred-event="7"]')).toBeAttached();
  expect(requests).toHaveLength(1);
  await page.locator('[data-deferred-event="7"] summary').click();
  await expect(page.locator('.event-fragment pre')).toContainText('FIRST');
  expect(requests).toHaveLength(2);
  await page.getByRole('button', { name: 'Next fragment', exact: true }).click();
  await expect(page.getByRole('alert')).toContainText('Fragment unavailable');
  await page.waitForTimeout(250);
  expect(requests).toHaveLength(3);
  fragmentFails = false;
  await page.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect(page.locator('.event-fragment pre')).toContainText('SECOND');
  await expect(page.locator('.event-fragment pre')).not.toContainText('FIRST');
  expect(await page.locator('.event-fragment pre').textContent()).toHaveLength(65536);
  await expect.poll(() => page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: { getQueryCache(): { findAll(filter: unknown): unknown[] } } }).documentQueryClient;
    return client.getQueryCache().findAll({ queryKey: ['doc-event-fragment'] }).length;
  })).toBe(1);
  await page.getByRole('button', { name: 'Previous fragment', exact: true }).click();
  await expect(page.locator('.event-fragment pre')).toContainText('FIRST');
  expect(requests.filter(url => url.includes('view=raw'))).toHaveLength(0);
  expect(requests.slice(1).every(url => url.includes('index=7') && url.includes('budget=65536'))).toBe(true);
  await page.locator('[data-deferred-event="7"] > summary').click();
  await expect(page.locator('.event-fragment')).toHaveCount(0);
  expect(errors).toEqual([]);
});

for (const result of ['deferred', '413'] as const) test(`raw ${result} switches once to fragments without audit reads`, async ({ page }) => {
  const requests: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url.pathname + url.search);
    if (url.pathname.endsWith('/event-fragments')) {
      await route.fulfill({ json: { event_index: 0, byte_offset: 0, total_bytes: 2, next: -1, json_fragment: '{}' } });
    } else if (url.searchParams.get('view') === 'raw' && result === '413') {
      await route.fulfill({ status: 413, json: { error: { message: 'Use fragments' } } });
    } else await route.fulfill({ json: eventPage([tool(0)]) });
  });
  const errors = await harness(page, '?mode=all');
  await page.locator('[data-deferred-event="0"] summary').click();
  await expect(page.locator('.event-fragment pre')).toHaveText('{}');
  await expect(page.getByRole('button', { name: 'Next fragment', exact: true })).toBeDisabled();
  expect(requests).toHaveLength(3);
  expect(requests[1]).toContain('view=raw&offset=0&limit=1');
  expect(errors).toEqual([]);
});

test('lite pending updates never replace completed joins in the independent full graph cache', async ({ page }) => {
  let pending = pendingFixture('first-document');
  const full = fullFixture(pending);
  full.graph.operations.merges = [{
    id: 'completed-join', event_id: 'completed-join', before: 'base', after: 'merged', source: 'topic', branch: 'main',
    scope: 'main', from: 'topic', created_at: '2026-10-14T00:00:00Z', historical_only: false, withdrawn: false,
    represented_grafts: [], redirect_children: [],
  }];
  let releaseFullPending!: () => void;
  const fullPendingGate = new Promise<void>(resolve => { releaseFullPending = resolve; });
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/pending-view')) {
      if (url.searchParams.get('projection') === 'on-hold-v1') await route.fulfill({ json: pendingStateFixture(pending) });
      else {
        await fullPendingGate;
        await route.fulfill({ json: wireFixture({ ...pending, graph: { ...full.graph, revision: pending.revision } }) });
      }
    } else if (url.pathname.endsWith('/view')) await route.fulfill({ json: wireFixture(full) });
    else if (url.pathname.endsWith('/revision')) await route.fulfill({ json: pending.revision });
    else await route.fulfill({ status: 404, json: {} });
  });
  const errors = await harness(page, '?repository');
  try {
    await expect(page.getByTestId('full-graph')).toContainText('completed-join');
    await expect(page.getByTestId('pending-state')).toContainText('first-document');
    pending = pendingFixture('next-document', { graph: '1', pending: '3' });
    await page.evaluate(revision => {
      (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(revision);
    }, pending.revision);
    await expect(page.getByTestId('pending-state')).toContainText('next-document');
    await expect(page.getByTestId('full-graph')).toContainText('completed-join');
    const caches = await page.evaluate(() => {
      const client = (window as unknown as { documentQueryClient: { getQueryData(key: unknown[]): unknown } }).documentQueryClient;
      return { full: client.getQueryData(['repo-view', 'repo']), lite: client.getQueryData(['pending-state', 'repo', 'on-hold-v1']) };
    });
    expect(caches.full).toMatchObject({ revision: { pending: '2' }, graph: { operations: { merges: [{ id: 'completed-join' }] } } });
    expect(caches.lite).toMatchObject({ projection: 'on-hold-v1', revision: { pending: '3' } });
    expect(caches.lite).not.toHaveProperty('graph');
    expect(errors).toEqual([]);
  } finally { releaseFullPending(); }
});

for (const fullResult of ['delayed', 'failure'] as const) test(`OnHold sessions and live updates do not wait for a ${fullResult} full graph`, async ({ page }) => {
  let pending: PendingView = pendingFixture('first-document');
  let fullFinished = false;
  let releaseFull!: () => void;
  const fullGate = new Promise<void>(resolve => { releaseFull = resolve; });
  let pendingReads = 0;
  const documents: string[] = [];
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/pending-view')) {
      pendingReads++;
      await route.fulfill({ json: url.searchParams.get('projection') === 'on-hold-v1' ? pendingStateFixture(pending) : wireFixture(pending) });
    } else if (url.pathname.endsWith('/view')) {
      if (fullResult === 'delayed') await fullGate;
      fullFinished = true;
      await route.fulfill(fullResult === 'failure'
        ? { status: 503, json: { error: { message: 'Full graph unavailable' } } }
        : { json: wireFixture(fullFixture(pendingFixture('old-document', { graph: '1', pending: '1' }))) });
    } else if (url.pathname.endsWith('/revision')) await route.fulfill({ json: pending.revision });
    else if (url.pathname.endsWith('/events')) {
      const hash = url.pathname.split('/').at(-2)!;
      documents.push(hash);
      await route.fulfill({ json: eventPage([message(0, `Conversation ${hash}`)], 0, 1, 0, hash) });
    } else if (url.pathname.endsWith('/me')) await route.fulfill({ status: 401, json: {} });
    else await route.fulfill({ status: 404, json: { error: { message: url.pathname } } });
  });
  const errors = await harness(page, '?onhold');
  try {
    await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(1);
    if (fullResult === 'delayed') expect(fullFinished).toBe(false);
    await page.locator('.pending-sessions .commit-row').click();
    await expect(page.getByText('Conversation first-document', { exact: true })).toBeVisible();
    pending = pendingFixture('next-document', { graph: '1', pending: '3' });
    await page.evaluate(revision => {
      (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(revision);
    }, pending.revision);
    await expect(page.getByText('Conversation next-document', { exact: true })).toBeVisible();
    expect(pendingReads).toBeGreaterThanOrEqual(2);
    if (fullResult === 'delayed') {
      releaseFull();
      await expect.poll(() => fullFinished).toBe(true);
      await expect(page.getByText('Conversation next-document', { exact: true })).toBeVisible();
      expect(documents).not.toContain('old-document');
    }
    expect(errors).toEqual([]);
  } finally { releaseFull(); }
});

test('OnHold retains the selected snapshot when the lite session disappears before the full graph recovers', async ({ page }) => {
  let pending = pendingStateFixture(pendingFixture('selected-document'));
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/pending-view') && url.searchParams.get('projection') === 'on-hold-v1') {
      await route.fulfill({ json: pending });
    } else if (url.pathname.endsWith('/revision')) await route.fulfill({ json: pending.revision });
    else if (url.pathname.endsWith('/events')) {
      await route.fulfill({ json: eventPage([message(0, 'Selected conversation remains readable')], 0, 1, 0, 'selected-document') });
    } else await route.fulfill({ status: 503, json: { error: { message: 'Full graph unavailable' } } });
  });
  const errors = await harness(page, '?onhold');
  await page.locator('.pending-sessions .commit-row').click();
  await expect(page.getByText('Selected conversation remains readable', { exact: true })).toBeVisible();
  pending = { ...pending, revision: { graph: '2', pending: '3' }, pending: [], snapshots: [], orphan_sessions: [], hold_counts: {} };
  await page.evaluate(revision => {
    (window as unknown as { emitRepositoryRevision(revision: unknown): void }).emitRepositoryRevision(revision);
  }, pending.revision);
  await expect(page.locator('.pending-sessions .commit-row')).toHaveCount(0);
  await expect(page.getByText('Selected conversation remains readable', { exact: true })).toBeVisible();
  await expect(page.locator('.live-viewer-status')).toHaveCount(0);
  expect(errors).toEqual([]);
});

test('readonly graph modes and zoom survive the first live branch ref', async ({ page }) => {
  const errors = await harness(page, '?graphIdentity');
  await page.locator('.graph-view-toggle button').last().click();
  await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
  await expect(page.locator('.graph-viewport')).toHaveAttribute('data-graph-view', 'prs');
  await expect(page.locator('.graph-viewport')).toHaveAttribute('data-graph-zoom', '125');
  await page.getByRole('button', { name: 'Publish first branch', exact: true }).click();
  await expect(page.locator('.graph-viewport')).toHaveAttribute('data-graph-view', 'prs');
  await expect(page.locator('.graph-viewport')).toHaveAttribute('data-graph-zoom', '125');
  expect(errors).toEqual([]);
});
