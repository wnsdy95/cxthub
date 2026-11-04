import { test, expect, type Page } from '@playwright/test';
import { clippedFoldPage, clippedPromptPage, foldPage, heavyVisiblePage, visibleMessage } from '../tests/documentVisibleFixture';
import type { DocVisiblePage } from '../src/documentVisible';

const documentWindowEnabled = process.env.CXT_DOCUMENT_WINDOW === '1' || process.env.CXT_DOCUMENT_BASELINE === '1';

async function harness(page: Page, query = '') {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/__visible-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  await page.goto(`/__visible-harness${query}`);
  return errors;
}

function singlePage(hash = 'document', text = 'Terminal document'): DocVisiblePage {
  return { ...heavyVisiblePage(), hash, total: 1, from: 0, to: 1, next_cursor: null,
    units: [{ from: 0, to: 1, turn_index: 0, event: visibleMessage(0, text, 'user'), fold: null }] };
}

for (const pageCount of [10, 100]) test(`document render baseline records ${pageCount} visited pages`, async ({ page }, testInfo) => {
  test.skip(process.env.CXT_DOCUMENT_BASELINE !== '1', 'Explicit local rendering count gate');
  test.setTimeout(180_000);
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    const cursor = url.searchParams.get('cursor');
    const from = cursor ? Number(cursor.replace('main-', '')) : 0;
    return route.fulfill({ json: heavyVisiblePage(from, pageCount * 620) });
  });
  const errors = await harness(page);
  for (let visited = 1; visited <= pageCount; visited++) {
    if (visited > 1) {
      await expect.poll(async () => {
        const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
        if (await target.count()) return 1;
        await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
        return target.count();
      }).toBe(1);
    } else await expect(page.locator('[data-visible-unit="589"]')).toBeAttached();
  }
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  await expect.poll(() => requests.length).toBe(pageCount);
  const observation = await page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: {
      getQueryCache(): { getAll(): { meta?: { documentBody?: boolean }; state: { data?: { pages?: { initial?: unknown }[] } } }[] };
    } }).documentQueryClient;
    return {
      mountedUnits: document.querySelectorAll('[data-visible-unit]').length,
      documentNodes: document.querySelector('[data-testid="document-scroll"]')!.querySelectorAll('*').length,
      htmlBytes: new TextEncoder().encode(document.querySelector('[data-testid="document-scroll"]')!.innerHTML).length,
      retainedRecords: client.getQueryCache().getAll().reduce((total, query) => total + (query.state.data?.pages?.length ?? 0), 0),
      retainedBodies: client.getQueryCache().getAll().reduce((total, query) => total +
        (query.meta?.documentBody && query.state.data ? 1 : 0) + (query.state.data?.pages?.filter(record => record.initial).length ?? 0), 0),
    };
  });
  expect(errors).toEqual([]);
  expect(requests.every(url => url.searchParams.get('projection') === 'visible-v1')).toBe(true);
  expect(observation.retainedRecords).toBe(pageCount);
  expect(observation.retainedBodies).toBeLessThanOrEqual(8);
  const report = { fixture: '20 anchors / 620 raw events per page, chat, closed folds; fresh query client and terminal page',
    observations: [{ visited: pageCount, requests: requests.length, ...observation }],
    limitations: 'Shared eight-entry body cache including the first page in this closed-fold workload. Active readers are pinned; find and broader active workloads remain. No heap, latency, frame-rate or production performance claim' };
  console.log(JSON.stringify(report));
  await testInfo.attach('document-render-baseline', { body: JSON.stringify(report, null, 2), contentType: 'application/json' });
  await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = 0; });
  await expect(page.locator('[data-visible-unit="0"]')).toBeVisible();
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  expect(requests).toHaveLength(pageCount + 1);
  expect(requests.at(-1)!.searchParams.get('cursor')).toBeNull();
});

test.describe('windowed document candidate', () => {
test.skip(!documentWindowEnabled, 'Opt-in until cache, keyboard traversal and find gates pass');

for (const pageCount of [10, 100]) test(`self replay bounds all ${pageCount} page bodies including the first`, async ({ page }, testInfo) => {
  test.setTimeout(180_000);
  const cursors: (string | null)[] = [];
  await page.route('**/api/v1/**', route => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    cursors.push(cursor);
    const from = Number(cursor?.replace('main-', '') ?? 0);
    return route.fulfill({ json: { ...heavyVisiblePage(from, pageCount * 620), replay_cursor: `main-${from}` } });
  });
  const errors = await harness(page);
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 1; visited <= pageCount; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect.poll(() => cursors.length).toBe(pageCount);
  const observation = await page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: {
      getQueryCache(): { getAll(): { meta?: { documentBody?: boolean }; state: { data?: { pages?: { initial?: unknown }[] } } }[] };
    } }).documentQueryClient;
    const queries = client.getQueryCache().getAll();
    return { mountedUnits: document.querySelectorAll('[data-visible-unit]').length,
      documentNodes: document.querySelector('[data-testid="document-scroll"]')!.querySelectorAll('*').length,
      pinnedInitialBodies: queries.reduce((count, query) => count + (query.state.data?.pages?.filter(record => record.initial).length ?? 0), 0),
      retainedBodies: queries.filter(query => query.meta?.documentBody && query.state.data).length };
  });
  expect(observation.pinnedInitialBodies).toBe(0);
  expect(observation.retainedBodies).toBeLessThanOrEqual(8);
  expect(observation.mountedUnits).toBeLessThanOrEqual(60);
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(page.locator('[data-visible-unit="0"]')).toBeVisible();
  expect(cursors.filter(cursor => cursor === null)).toHaveLength(1);
  expect(cursors.filter(cursor => cursor === 'main-0')).toHaveLength(1);
  const report = { visited: pageCount, forwardRequests: pageCount, firstReplayRequests: 1, ...observation };
  console.log(JSON.stringify(report));
  await testInfo.attach('document-self-replay-retention', { body: JSON.stringify(report, null, 2), contentType: 'application/json' });
  expect(errors).toEqual([]);
});

test('an evicted first page fails closed when its generation changes without sending a new initial request', async ({ page }) => {
  const cursors: (string | null)[] = [];
  await page.route('**/api/v1/**', route => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor');
    cursors.push(cursor);
    if (cursor === 'main-0') return route.fulfill({ status: 409, json: { error: { code: 'doc_visible_generation_stale', message: 'Original generation expired' } } });
    const from = Number(cursor?.replace('main-', '') ?? 0);
    return route.fulfill({ json: { ...heavyVisiblePage(from, 7440), replay_cursor: `main-${from}` } });
  });
  const errors = await harness(page);
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 1; visited <= 12; visited++) {
    await expect.poll(async () => {
      if (await page.locator(`[data-visible-unit="${visited * 620 - 31}"]`).count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return page.locator(`[data-visible-unit="${visited * 620 - 31}"]`).count();
    }).toBe(1);
  }
  await scroll.evaluate(element => { element.scrollTop = 0; });
  const first = page.locator('[data-document-page="units:0"]');
  await expect(first.getByRole('alert')).toContainText('Original generation expired');
  await expect(first.getByRole('button', { name: 'Restart document', exact: true })).toBeVisible();
  expect(cursors.filter(cursor => cursor === null)).toHaveLength(1);
  expect(cursors.filter(cursor => cursor === 'main-0')).toHaveLength(1);
  expect(errors).toEqual([]);
});

for (const failure of ['none', 'changed', 'denied'] as const) test(`evicted middle page restores only its pinned cursor (${failure})`, async ({ page }) => {
  const cursors: string[] = [];
  await page.route('**/api/v1/**', route => {
    const cursor = new URL(route.request().url()).searchParams.get('cursor') ?? '';
    cursors.push(cursor);
    const from = Number(cursor.replace('main-', ''));
    const replay = cursor === 'main-620' && cursors.filter(value => value === cursor).length > 1;
    if (replay && failure === 'denied') return route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Replay access denied' } } });
    const body = { ...heavyVisiblePage(from, 7440), replay_cursor: `main-${from}` };
    if (replay && failure === 'changed') body.generation = 'new-generation';
    return route.fulfill({ json: body });
  });
  const errors = await harness(page);
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 1; visited <= 12; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  const recycled = page.locator('[data-document-page="units:620"]');
  await expect(recycled).toHaveAttribute('data-page-mounted', 'false');
  await recycled.evaluate(element => element.scrollIntoView({ block: 'start' }));
  await expect.poll(() => cursors.filter(value => value === 'main-620').length).toBe(2);
  if (failure === 'none') await expect(recycled.locator('[data-visible-unit="620"]')).toBeVisible();
  else {
    await expect(recycled.getByRole('alert')).toBeVisible();
    await expect(recycled.locator('[data-visible-unit]')).toHaveCount(0);
    await expect(recycled.getByRole('button', { name: failure === 'denied' ? 'Retry' : 'Restart document', exact: true })).toBeVisible();
    await page.getByRole('combobox').focus();
    await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
    await expect(recycled).toHaveAttribute('data-page-mounted', 'false');
    await recycled.evaluate(element => element.scrollIntoView({ block: 'start' }));
    await expect(recycled.getByRole('alert')).toBeVisible();
    expect(cursors.filter(value => value === 'main-620')).toHaveLength(2);
  }
  expect(cursors.filter(value => value === '')).toHaveLength(1);
  expect(errors).toEqual([]);
});

test('raw detail denial survives recycling and elapsed query GC until explicit retry', async ({ page }) => {
  let reads = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.searchParams.get('view') === 'raw' && url.searchParams.get('offset') === '30') {
      reads++;
      return route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Raw detail denied' } } });
    }
    const from = Number(url.searchParams.get('cursor')?.replace('main-', '') ?? 0);
    const body = heavyVisiblePage(from, 2480);
    if (from === 0) body.units[0].event = { ...body.units[0].event!, deferred: true, event_bytes: 1000, blocks: undefined };
    return route.fulfill({ json: body });
  });
  const errors = await harness(page);
  const detail = page.locator('[data-deferred-event="30"]');
  await detail.locator('summary').click();
  await expect(detail.getByRole('alert')).toContainText('Raw detail denied');
  await page.getByRole('combobox').focus();
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 2; visited <= 4; visited++) {
    await expect.poll(async () => {
      if (await page.locator(`[data-visible-unit="${visited * 620 - 31}"]`).count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return page.locator(`[data-visible-unit="${visited * 620 - 31}"]`).count();
    }).toBe(1);
  }
  await expect(detail).not.toBeAttached();
  await page.clock.install();
  await page.clock.fastForward(65_000);
  await page.clock.resume();
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(detail.getByRole('alert')).toContainText('Raw detail denied');
  expect(reads).toBe(1);
  await detail.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect.poll(() => reads).toBe(2);
  expect(errors).toEqual([]);
});

for (const width of [390, 1280]) test(`old visible document restores folds and details across backscroll and resize at ${width}px`, async ({ page }) => {
  await page.setViewportSize({ width, height: 800 });
  const requests: URL[] = [];
  const first = heavyVisiblePage(0, 6200);
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.searchParams.has('expand')) {
      const detail = foldPage(first, 0, 0, 30);
      detail.events[0] = { ...detail.events[0], kind: 'tool_result', deferred: false, output: 'Expanded result survives recycling' };
      return route.fulfill({ json: detail });
    }
    const from = Number(url.searchParams.get('cursor')?.replace('main-', '') ?? 0);
    return route.fulfill({ json: from ? heavyVisiblePage(from, 6200) : first });
  });
  const errors = await harness(page);
  const unit = page.locator('[data-visible-unit="0"]');
  await unit.locator(':scope > button').click();
  const result = unit.locator('[data-event-index="0"] details');
  await result.locator('summary').click();
  await expect(result).toHaveAttribute('open', '');
  await page.getByRole('combobox', { name: 'View mode' }).focus();
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 2; visited <= 10; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect(unit).not.toBeAttached();
  await page.setViewportSize({ width: width === 390 ? 1280 : 390, height: 800 });
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(unit).toBeVisible();
  await expect(unit.locator(':scope > button')).toHaveAttribute('aria-expanded', 'true');
  await expect(result).toHaveAttribute('open', '');
  await expect(result.locator('pre')).toBeVisible();
  await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBe(0);
  expect(requests.filter(url => url.searchParams.has('expand'))).toHaveLength(2);
  expect(requests.filter(url => !url.searchParams.has('expand'))).toHaveLength(11);
  const initialReads = requests.filter(url => !url.searchParams.has('expand') && !url.searchParams.has('cursor'));
  expect(initialReads).toHaveLength(2);
  expect(initialReads[1].search).toBe(initialReads[0].search);
  expect(errors).toEqual([]);
});

test('document window restores a deferred fragment position without rereading its first fragment', async ({ page }) => {
  const fragments: number[] = [];
  const text = 'line\n'.repeat(2000);
  const fragmentSize = text.length;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/event-fragments')) {
      const offset = Number(url.searchParams.get('offset'));
      fragments.push(offset);
      return route.fulfill({ json: { event_index: 30, byte_offset: offset, total_bytes: fragmentSize * 3,
        next: offset === fragmentSize * 2 ? -1 : offset + fragmentSize, json_fragment: text } });
    }
    const from = Number(url.searchParams.get('cursor')?.replace('main-', '') ?? 0);
    const result = heavyVisiblePage(from, 6200);
    if (from === 0) result.units[0].event = { ...result.units[0].event!, deferred: true, event_bytes: 200_000, blocks: undefined };
    return route.fulfill({ json: result });
  });
  const errors = await harness(page);
  const detail = page.locator('[data-deferred-event="30"]');
  await detail.locator('summary').click();
  await detail.getByRole('button', { name: 'Next fragment' }).click();
  await expect(detail.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentSize));
  await detail.locator('pre').evaluate(element => { element.scrollTop = 500; });
  await expect.poll(() => detail.locator('pre').evaluate(element => element.scrollTop)).toBe(500);
  await page.getByRole('combobox').focus();
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 2; visited <= 10; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect(detail).not.toBeAttached();
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(detail).toHaveAttribute('open', '');
  await expect(detail.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentSize));
  await expect(detail.locator('pre')).toHaveText(text);
  await expect.poll(() => detail.locator('pre').evaluate(element => element.scrollTop)).toBe(500);
  expect(fragments).toEqual([0, fragmentSize, fragmentSize]);
  await detail.getByRole('button', { name: 'Previous fragment' }).click();
  await expect(detail.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', '0');
  await expect.poll(() => detail.locator('pre').evaluate(element => element.scrollTop)).toBe(0);
  expect(errors).toEqual([]);
});

test('document window preserves keyboard focus offscreen until focus leaves the page', async ({ page }) => {
  await page.route('**/api/v1/**', route => {
    const from = Number(new URL(route.request().url()).searchParams.get('cursor')?.replace('main-', '') ?? 0);
    return route.fulfill({ json: heavyVisiblePage(from, 2480) });
  });
  const errors = await harness(page);
  const focused = page.locator('[data-visible-unit="0"] > button');
  await focused.focus();
  for (let visited = 2; visited <= 4; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect(focused).toBeFocused();
  await page.getByRole('combobox').focus();
  await expect(focused).not.toBeAttached();
  expect(errors).toEqual([]);
});

test('returning to an offscreen fold preserves its terminal error without recapturing', async ({ page }) => {
  const foldReads: string[] = [];
  const first = heavyVisiblePage(0, 2480);
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    const expand = url.searchParams.get('expand');
    if (expand) {
      foldReads.push(expand);
      return expand === 'fold-0' ? route.fulfill({ json: foldPage(first, 0, 0, 10) })
        : route.fulfill({ status: 403, json: { error: { code: 'access_revoked', message: 'Fold access revoked' } } });
    }
    const from = Number(url.searchParams.get('cursor')?.replace('main-', '') ?? 0);
    return route.fulfill({ json: from ? heavyVisiblePage(from, 2480) : first });
  });
  const errors = await harness(page);
  const unit = page.locator('[data-visible-unit="0"]');
  await unit.locator(':scope > button').click();
  await unit.getByRole('button', { name: 'Load more events', exact: true }).click();
  await expect(unit.getByRole('alert')).toContainText('Fold access revoked');
  await page.getByRole('combobox').focus();
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 2; visited <= 4; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect(unit).not.toBeAttached();
  await page.clock.install();
  await page.clock.fastForward(65_000);
  await page.clock.resume();
  await scroll.evaluate(element => { element.scrollTop = 0; });
  await expect(unit.getByRole('alert')).toContainText('Fold access revoked');
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(10);
  expect(foldReads).toEqual(['fold-0', 'fold-next-10']);
  await unit.getByRole('button', { name: 'Retry', exact: true }).click();
  await expect.poll(() => foldReads.length).toBe(3);
  expect(foldReads[2]).toBe('fold-next-10');
  expect(errors).toEqual([]);
});

test('resizing around a long message keeps the currently read event anchored', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.route('**/api/v1/**', route => {
    const from = Number(new URL(route.request().url()).searchParams.get('cursor')?.replace('main-', '') ?? 0);
    const result = heavyVisiblePage(from, 2480);
    result.units[4].event!.blocks = [{ type: 'text', text: 'A long paragraph that reflows when the viewport becomes narrow. '.repeat(80) }];
    return route.fulfill({ json: result });
  });
  const errors = await harness(page);
  const scroll = page.getByTestId('document-scroll');
  await expect.poll(async () => {
    const target = page.locator('[data-visible-unit="1209"]');
    if (await target.count()) return 1;
    await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
    return target.count();
  }).toBe(1);
  const anchor = page.locator('[data-visible-unit="930"]');
  await anchor.evaluate(element => element.scrollIntoView({ block: 'start' }));
  const before = await anchor.evaluate(element => element.getBoundingClientRect().top);
  await page.setViewportSize({ width: 390, height: 800 });
  await expect.poll(() => anchor.evaluate((element, expected) => Math.abs(element.getBoundingClientRect().top - expected), before)).toBeLessThan(2);
  expect(errors).toEqual([]);
});

test('responsive context switches from pane scrolling to window scrolling without mounting all pages', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.route('**/api/v1/**', route => {
    const from = Number(new URL(route.request().url()).searchParams.get('cursor')?.replace('main-', '') ?? 0);
    return route.fulfill({ json: heavyVisiblePage(from, 6200) });
  });
  const errors = await harness(page, '?responsive');
  const scroll = page.getByTestId('document-scroll');
  for (let visited = 2; visited <= 10; visited++) {
    await expect.poll(async () => {
      const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
      if (await target.count()) return 1;
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return target.count();
    }).toBe(1);
  }
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  await page.setViewportSize({ width: 390, height: 800 });
  await expect.poll(() => scroll.evaluate(element => getComputedStyle(element).overflowY)).toBe('visible');
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await expect(page.locator('[data-visible-unit="6169"]')).toBeVisible();
  await expect.poll(() => page.locator('[data-visible-unit]').count()).toBeLessThanOrEqual(60);
  expect(errors).toEqual([]);
});
});

test('document recycling stays disabled by default while release gates remain open', async ({ page }) => {
  test.skip(documentWindowEnabled, 'Default-path guard is separate from candidate runs');
  const requests: string[] = [];
  await page.route('**/api/v1/**', route => {
    requests.push(route.request().url());
    const from = Number(new URL(route.request().url()).searchParams.get('cursor')?.replace('main-', '') ?? 0);
    return route.fulfill({ json: heavyVisiblePage(from, 7440) });
  });
  const errors = await harness(page);
  for (let visited = 2; visited <= 12; visited++) {
    await expect.poll(async () => {
      const count = await page.locator('[data-visible-unit]').count();
      if (count < visited * 20) await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
      return count;
    }).toBe(visited * 20);
  }
  await expect(page.locator('[data-document-page]')).toHaveCount(0);
  expect(requests).toHaveLength(12);
  expect(errors).toEqual([]);
});

test('twenty anchors skip tools; one bottom sentinel preloads, local fold chunks never fan out, and terminal pages load children', async ({ page }) => {
  const requests: URL[] = [];
  const first = heavyVisiblePage();
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.includes('/tail/')) return route.fulfill({ json: { ...singlePage('tail', 'Child document loaded'), base: 'document' } });
    if (url.searchParams.has('expand')) return route.fulfill({ json: url.searchParams.get('expand') === 'fold-0'
      ? foldPage(first, 0, 0, 10) : foldPage(first, 0, 10, 30) });
    return route.fulfill({ json: url.searchParams.has('cursor') ? heavyVisiblePage(620) : first });
  });
  const errors = await harness(page, '?tail');
  await expect(page.locator('[data-visible-unit]')).toHaveCount(20);
  await expect(page.locator('[data-event-index]')).toHaveCount(20);
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(1);
  expect(requests).toHaveLength(1);
  expect(Object.fromEntries(requests[0].searchParams)).toEqual({ projection: 'visible-v1', view: 'chat', offset: '-1', limit: '20', base: 'base' });
  const unit = page.locator('[data-visible-unit="0"]');
  await unit.getByRole('button').click();
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(10);
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(1);
  expect(requests).toHaveLength(2);
  expect(Object.fromEntries(requests[1].searchParams)).toEqual({ projection: 'visible-v1', expand: 'fold-0' });
  await unit.getByRole('button', { name: 'Load more events', exact: true }).click();
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(30);
  expect(Object.fromEntries(requests[2].searchParams)).toEqual({ projection: 'visible-v1', expand: 'fold-next-10' });
  await unit.locator(':scope > button').click();
  await unit.locator(':scope > button').click();
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(30);
  await page.waitForTimeout(200);
  expect(requests).toHaveLength(3);
  expect(requests.some(url => url.searchParams.get('view') === 'raw')).toBe(false);
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight - element.clientHeight - 200; });
  await expect.poll(() => requests.length).toBe(4);
  expect(Object.fromEntries(requests[3].searchParams)).toEqual({ projection: 'visible-v1', cursor: 'main-620' });
  await expect(page.locator('[data-visible-unit]')).toHaveCount(20);
  await expect(page.getByTestId('continuation')).toHaveCount(0);
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.locator('[data-visible-unit]')).toHaveCount(40);
  await expect(page.getByTestId('continuation')).toHaveCount(0);
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(1);
  await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.getByText('Child document loaded', { exact: true })).toBeVisible();
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(0);
  expect(requests.filter(url => url.searchParams.has('base'))).toHaveLength(2);
  expect(requests).toHaveLength(5);
  expect(errors).toEqual([]);
});

test('prompt folds are clipped to selection, preserve inherited styling and flags, and reveal deferred messages without raw auto-fetch', async ({ page }) => {
  const context = clippedPromptPage(5);
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    const expand = url.searchParams.get('expand');
    return route.fulfill({ json: expand ? clippedFoldPage(context, expand === 'clipped-fold' ? 5 : 15, expand === 'clipped-fold' ? 15 : 30) : context });
  });
  const errors = await harness(page, '?mode=prompts&start=5');
  await expect(page.getByText('Actual prompt', { exact: true })).toBeVisible();
  await expect(page.locator('[data-event-index]')).toHaveCount(1);
  await expect(page.locator('[data-visible-unit="5"]')).toHaveAttribute('data-turn-index', '0');
  expect(requests).toHaveLength(1);
  const leading = page.locator('[data-visible-unit="5"]');
  await leading.locator(':scope > button').click();
  await expect(page.locator('[data-deferred-event]')).toHaveCount(9);
  await expect(page.locator('[data-event-index="5"]')).toHaveClass('doc-inherited');
  await expect(page.locator('[data-event-index="10"]')).not.toHaveClass('doc-inherited');
  await expect(page.locator('[data-deferred-event="10"] summary')).toContainText('seed');
  await expect(page.locator('[data-event-index="11"]')).toHaveCount(0);
  await expect(page.locator('[data-event-index="0"]')).toHaveCount(0);
  await leading.getByRole('button', { name: 'Load more events', exact: true }).click();
  await expect(page.locator('[data-deferred-event]')).toHaveCount(24);
  await leading.locator(':scope > button').click();
  await leading.locator(':scope > button').click();
  await expect(page.locator('[data-deferred-event]')).toHaveCount(24);
  await page.waitForTimeout(200);
  expect(requests).toHaveLength(3);
  expect(requests.every(url => url.searchParams.get('projection') === 'visible-v1')).toBe(true);
  expect(errors).toEqual([]);
});

for (const failure of ['context', 'overlap', 'gap', 'wrong-kind', 'legacy', 'stale'] as const) {
  test(`rejects ${failure} continuation without appending or falling back; restart captures a new selection`, async ({ page }) => {
    let restarted = false;
    const requests: URL[] = [];
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      requests.push(url);
      if (!url.searchParams.has('cursor')) return route.fulfill({ json: restarted ? singlePage('document', 'Restarted safely') : heavyVisiblePage() });
      if (failure === 'stale') return route.fulfill({ status: 409, json: { error: { code: 'doc_visible_generation_stale', message: 'Generation changed' } } });
      const next = heavyVisiblePage(620);
      if (failure === 'context') next.generation = 'wrong';
      if (failure === 'overlap') next.from--;
      if (failure === 'gap') next.from++;
      if (failure === 'wrong-kind') next.kind = 'fold';
      return route.fulfill({ json: failure === 'legacy' ? { hash: 'document', envelope: next.envelope, offset: 620, total: 621, next: -1, inherited: 0,
        events: [visibleMessage(620, 'Mixed legacy')] } : next });
    });
    const errors = await harness(page);
    await expect(page.locator('[data-visible-unit]')).toHaveCount(20);
    await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
    await expect(page.getByRole('alert')).toBeVisible();
    await expect(page.locator('[data-visible-unit]')).toHaveCount(20);
    await expect(page.getByText('Mixed legacy', { exact: true })).toHaveCount(0);
    await page.waitForTimeout(250);
    expect(requests).toHaveLength(2);
    restarted = true;
    await page.getByRole('button', { name: 'Restart document', exact: true }).click();
    await expect(page.getByText('Restarted safely', { exact: true })).toBeVisible();
    await expect(page.locator('[data-visible-unit]')).toHaveCount(1);
    expect(requests).toHaveLength(3);
    expect(requests[2].search).toBe(requests[0].search);
    expect(errors).toEqual([]);
  });
}

test('a malformed fold continuation stays local and never replaces accepted rows', async ({ page }) => {
  const context = heavyVisiblePage();
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    const expand = url.searchParams.get('expand');
    const body = expand ? foldPage(context, 0, expand === 'fold-0' ? 0 : 9, expand === 'fold-0' ? 10 : 30) : context;
    return route.fulfill({ json: body });
  });
  const errors = await harness(page);
  const unit = page.locator('[data-visible-unit="0"]');
  await unit.getByRole('button').click();
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(10);
  await unit.getByRole('button', { name: 'Load more events', exact: true }).click();
  await expect(unit.getByRole('alert')).toBeVisible();
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(10);
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(1);
  expect(requests).toHaveLength(3);
  expect(errors).toEqual([]);
});

for (const kind of ['unsupported', 'older-server'] as const) {
  test(`${kind} falls back as a whole stream without re-requesting a valid legacy initial`, async ({ page }) => {
    const requests: URL[] = [];
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      requests.push(url);
      if (kind === 'unsupported' && url.searchParams.has('projection')) return route.fulfill({ status: 409,
        json: { error: { code: 'doc_visible_projection_unsupported', message: 'Legacy document' } } });
      const offset = Math.max(0, Number(url.searchParams.get('offset')));
      return route.fulfill({ json: { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' },
        offset, total: 2, inherited: 0, next: offset === 0 ? 1 : -1, events: [visibleMessage(offset, `Legacy ${offset}`, 'user')] } });
    });
    const errors = await harness(page);
    await expect(page.getByText('Legacy 1', { exact: true })).toBeVisible();
    expect(requests).toHaveLength(kind === 'unsupported' ? 3 : 2);
    expect(requests.filter(url => url.searchParams.has('projection'))).toHaveLength(1);
    expect(Object.fromEntries(requests.at(-1)!.searchParams)).toEqual({ view: 'chat', offset: '1', limit: '20' });
    await expect(page.locator('[data-document-projection]')).toHaveCount(0);
    expect(errors).toEqual([]);
  });
}

for (const failure of [{ status: 401, code: 'doc_visible_projection_unsupported' }, { status: 403, code: 'doc_visible_projection_unsupported' },
  { status: 422, code: 'integrity' }, { status: 409, code: 'doc_visible_generation_stale' }, { status: 503, code: 'doc_range_index_unavailable' }]) {
  test(`initial ${failure.status}/${failure.code} fails closed without fallback or automatic retry`, async ({ page }) => {
    const requests: URL[] = [];
    await page.route('**/api/v1/**', route => {
      requests.push(new URL(route.request().url()));
      return route.fulfill({ status: failure.status, json: { error: { code: failure.code, message: 'Read rejected' } } });
    });
    const errors = await harness(page);
    await expect(page.getByRole('alert')).toContainText('Read rejected');
    await page.waitForTimeout(250);
    expect(requests).toHaveLength(1);
    expect(requests[0].searchParams.get('projection')).toBe('visible-v1');
    expect(errors).toEqual([]);
  });
}

test('a fully inherited terminal document still admits its child exactly once', async ({ page }) => {
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    return route.fulfill({ json: url.pathname.includes('/tail/') ? { ...singlePage('tail', 'After empty parent'), base: 'document' }
      : { ...singlePage(), total: 50, inherited: 50, start: 50, from: 50, to: 50, units: [] } });
  });
  const errors = await harness(page, '?tail');
  await expect(page.getByText('After empty parent', { exact: true })).toBeVisible();
  await expect(page.locator('[data-document-sentinel]')).toHaveCount(0);
  expect(requests).toHaveLength(2);
  expect(errors).toEqual([]);
});

test('switching mode abandons a delayed visible request and captures the new full selection', async ({ page }) => {
  const requests: URL[] = [];
  let release!: () => void;
  const delayed = new Promise<void>(resolve => { release = resolve; });
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.searchParams.get('view') === 'chat') {
      await delayed;
      return route.fulfill({ json: heavyVisiblePage() });
    }
    return route.fulfill({ json: { ...singlePage('document', 'Only new mode'), view: 'prompts' } });
  });
  const errors = await harness(page);
  await expect.poll(() => requests.length).toBe(1);
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.getByText('Only new mode', { exact: true })).toBeVisible();
  release();
  await page.waitForTimeout(200);
  await expect(page.locator('[data-event-index]')).toHaveCount(1);
  expect(requests).toHaveLength(2);
  expect(Object.fromEntries(requests[1].searchParams)).toEqual({ projection: 'visible-v1', view: 'prompts', offset: '-1', limit: '20', base: 'base' });
  expect(errors).toEqual([]);
});

test('all view honors explicit false seed flags, hides zero-row reasoning, and keeps raw expansion user-driven', async ({ page }) => {
  const requests: URL[] = [];
  const literal = '[cxt seed] Branch-switch context: literal human prompt';
  const context: DocVisiblePage = { ...singlePage(), view: 'all', total: 4, to: 4, units: [
    { from: 0, to: 1, turn_index: -1, event: { ...visibleMessage(0, '', 'user'), deferred: true, seed_summary: true, blocks: undefined }, fold: null },
    { from: 1, to: 2, turn_index: -1, event: null, fold: { from: 1, to: 2, rows: 0, cursor: 'zero-rows' } },
    { from: 2, to: 3, turn_index: 2, event: visibleMessage(2, literal, 'user'), fold: null },
    { from: 3, to: 4, turn_index: 2, event: visibleMessage(3, 'Normal answer'), fold: null },
  ] };
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    return route.fulfill({ json: url.searchParams.get('view') === 'raw'
      ? { hash: 'document', envelope: context.envelope, offset: 0, total: 4, inherited: 0, next: 1,
        events: [{ kind: 'message', role: 'user', event_index: 0, blocks: [{ type: 'text', text: 'Seed without a prefix' }] }] }
      : context });
  });
  const errors = await harness(page, '?mode=all');
  await expect(page.getByText(literal, { exact: true })).toBeVisible();
  await expect(page.locator('[data-event-index]')).toHaveCount(3);
  await expect(page.locator('[data-visible-unit="1"]')).toHaveCount(0);
  await expect(page.locator('[data-event-index="2"] .compact-summary')).toHaveCount(0);
  expect(requests).toHaveLength(1);
  await page.locator('[data-deferred-event="0"] > summary').click();
  await expect(page.locator('[data-event-index="0"] .compact-summary summary')).toContainText('seed');
  expect(requests).toHaveLength(2);
  expect(Object.fromEntries(requests[1].searchParams)).toEqual({ view: 'raw', offset: '0', limit: '1' });
  expect(errors).toEqual([]);
});

for (const kind of ['next', 'fold'] as const) {
  test(`durable pending ${kind} projection retries the same selector, never recaptures or falls back`, async ({ page }) => {
    await page.clock.install({ time: new Date('2026-10-19T00:00:00Z') });
    await page.clock.pauseAt(new Date('2026-10-19T00:00:01Z'));
    await page.addInitScript(() => { Math.random = () => 0; });
    const requests: URL[] = [];
    const pending: string[] = [];
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      requests.push(url);
      if (url.searchParams.has(kind === 'fold' ? 'expand' : 'cursor')) {
        pending.push(url.search);
        if (pending.length <= 2) return route.fulfill({ status: 503, headers: { 'Retry-After': '3' },
          json: { error: { code: 'doc_range_index_pending', message: 'Preparing projection' } } });
        return route.fulfill({ json: kind === 'fold' ? foldPage(heavyVisiblePage(), 0) : heavyVisiblePage(620) });
      }
      return route.fulfill({ json: heavyVisiblePage() });
    });
    const errors = await harness(page);
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator('[data-visible-unit]').count();
    }).toBe(20);
    if (kind === 'fold') await page.locator('[data-visible-unit="0"] > button').click();
    else await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
    for (let failure = 1; failure <= 2; failure++) {
      await expect.poll(async () => {
        await page.clock.runFor(1);
        return page.evaluate(() => {
          const client = (window as unknown as { documentQueryClient: {
            getQueryCache(): { getAll(): { state: { fetchStatus: string; fetchFailureCount: number } }[] };
          } }).documentQueryClient;
          return client.getQueryCache().getAll().find(query => query.state.fetchStatus === 'fetching')?.state.fetchFailureCount;
        });
      }).toBe(failure);
      await page.clock.runFor(1);
      await expect(page.getByRole('alert')).toHaveCount(0);
      await page.clock.runFor(3000);
    }
    await expect.poll(async () => {
      await page.clock.runFor(1);
      return page.locator(kind === 'fold' ? '[data-deferred-event]' : '[data-visible-unit]').count();
    }).toBe(kind === 'fold' ? 30 : 40);
    expect(new Set(pending).size).toBe(1);
    expect(pending).toHaveLength(3);
    expect(requests.filter(url => url.searchParams.has('base'))).toHaveLength(1);
    expect(requests.every(url => url.searchParams.get('projection') === 'visible-v1')).toBe(true);
    await page.clock.runFor(120_000);
    expect(requests).toHaveLength(4);
    expect(errors).toEqual([]);
  });
}

for (const returning of [false, true]) {
  test(`empty all-view history caps each automatic burst at two pages${returning ? ' even with cached pages after mode changes' : ''}`, async ({ page }) => {
    const requests: URL[] = [];
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      requests.push(url);
      if (url.searchParams.get('view') === 'prompts') return route.fulfill({ json: { ...singlePage('document', 'Other view'), view: 'prompts' } });
      const from = Number(url.searchParams.get('cursor')?.replace('empty-', '') ?? 0);
      const to = from + 20;
      const context: DocVisiblePage = { ...heavyVisiblePage(), view: 'all', from, to, total: 120, next_cursor: to === 120 ? null : `empty-${to}`,
        units: Array.from({ length: 20 }, (_, position) => ({ from: from + position, to: from + position + 1, turn_index: -1, event: null,
          fold: { from: from + position, to: from + position + 1, rows: 0, cursor: `unused-fold-${from + position}` } })) };
      return route.fulfill({ json: context });
    });
    const errors = await harness(page, '?mode=all');
    const continueButton = page.locator('[data-document-sentinel]').getByRole('button', { name: 'Load more events', exact: true });
    await expect(continueButton).toBeVisible();
    await expect(page.locator('[data-visible-unit], .turn, [data-event-index], [data-document-fold]')).toHaveCount(0);
    await expect(page.locator('[data-document-sentinel]')).toHaveCount(1);
    await page.waitForTimeout(250);
    expect(requests).toHaveLength(2);
    await continueButton.click();
    await expect.poll(() => requests.length).toBe(4);
    await expect(continueButton).toBeVisible();
    await page.waitForTimeout(250);
    expect(requests).toHaveLength(4);
    if (returning) {
      await page.getByLabel('View mode').selectOption('prompts');
      await expect(page.getByText('Other view', { exact: true })).toBeVisible();
      await page.getByLabel('View mode').selectOption('all');
      await expect(continueButton).toBeVisible();
      await page.waitForTimeout(250);
      expect(requests).toHaveLength(5);
      await continueButton.click();
      await expect(continueButton).toBeVisible();
      await page.waitForTimeout(250);
      expect(requests).toHaveLength(5);
    }
    await continueButton.click();
    await expect(page.locator('[data-document-sentinel]')).toHaveCount(0);
    expect(requests).toHaveLength(returning ? 7 : 6);
    expect(requests.some(url => url.searchParams.has('expand') || url.searchParams.get('view') === 'raw')).toBe(false);
    expect(requests.filter(url => url.searchParams.get('view') === 'all')).toHaveLength(1);
    expect(errors).toEqual([]);
  });
}

test('restart never reuses an earlier restart cache after leaving and returning to a mode', async ({ page }) => {
  let captures = 0;
  const requests: string[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url.search);
    if (url.searchParams.has('cursor')) return route.fulfill({ status: 409,
      json: { error: { code: 'doc_visible_generation_stale', message: 'Restart this generation' } } });
    if (url.searchParams.get('view') === 'prompts') return route.fulfill({ json: { ...singlePage('document', 'Other mode'), view: 'prompts' } });
    captures++;
    return route.fulfill({ json: captures < 3 ? { ...heavyVisiblePage(), generation: `capture-${captures}` } : singlePage('document', 'Fresh third capture') });
  });
  const errors = await harness(page);
  await expect(page.locator('[data-visible-unit]')).toHaveCount(20);
  await page.getByTestId('document-scroll').evaluate(element => { element.scrollTop = element.scrollHeight; });
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByRole('button', { name: 'Restart document', exact: true }).click();
  await expect.poll(() => captures).toBe(2);
  await page.getByLabel('View mode').selectOption('prompts');
  await expect(page.getByText('Other mode', { exact: true })).toBeVisible();
  await page.getByLabel('View mode').selectOption('chat');
  await expect(page.getByRole('alert')).toBeVisible();
  expect(captures, JSON.stringify(requests)).toBe(2);
  await page.getByRole('button', { name: 'Restart document', exact: true }).click();
  await expect(page.getByText('Fresh third capture', { exact: true })).toBeVisible();
  expect(captures).toBe(3);
  expect(errors).toEqual([]);
});
