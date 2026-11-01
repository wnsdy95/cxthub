import { build } from 'esbuild';
import { test, expect, type CDPSession, type Page } from '@playwright/test';
import type { QueryClient } from '@tanstack/react-query';
import { heavyTextVisiblePage } from '../tests/documentVisibleFixture';
import { fragmentBytes, fragmentTotalBytes, fragmentText, memoryFold, rawResult } from '../tests/documentActiveFixture';
import { DOCUMENT_DETAIL_BYTES, DOCUMENT_FRAGMENT_BYTES, DOCUMENT_FRAGMENT_RESPONSE_BYTES, DOCUMENT_PAGE_BYTES } from '../src/documentReads';

const enabled = process.env.CXT_DOCUMENT_COST === '1';
const activeWorkload = process.env.CXT_DOCUMENT_COST_ACTIVE === '1';
const samples = Number(process.env.CXT_DOCUMENT_COST_SAMPLES ?? 3);
const variants = new Map<boolean, { script: string; styles: string }>();
const frames = 120;
type ActiveRead = { kind: string; index: number; offset: number; responseBytes: number; parameters: Record<string, string> };
test.use({ trace: 'off', video: 'off', screenshot: 'off' });

test.beforeAll(async () => {
  if (!enabled) return;
  expect(Number.isSafeInteger(samples) && samples >= 1 && samples <= 3).toBe(true);
  for (const windowed of [false, true]) {
    const bundle = await build({ entryPoints: ['e2e/document-cost-harness.ts'], bundle: true, write: false,
      outdir: 'document-cost-fixture', platform: 'browser', format: 'esm', jsx: 'automatic', minify: true,
      loader: { '.webp': 'dataurl', '.svg': 'dataurl' },
      define: { 'import.meta.env': JSON.stringify({ VITE_DOCUMENT_RENDER_WINDOW: windowed ? '1' : '0' }),
        'process.env.NODE_ENV': '"production"' } });
    variants.set(windowed, { script: bundle.outputFiles.find(output => output.path.endsWith('.js'))!.text,
      styles: bundle.outputFiles.find(output => output.path.endsWith('.css'))!.text });
  }
});

async function settle(page: Page) {
  await expect.poll(() => page.evaluate(() => (window as unknown as { documentQueryClient: QueryClient })
    .documentQueryClient.isFetching()), { timeout: 20_000 }).toBe(0);
  await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
}

async function snapshot(page: Page, session: CDPSession) {
  const state = await page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient;
    const queries = client.getQueryCache().getAll();
    const bodies = queries.filter(query => query.meta?.documentBody === true && query.state.data !== undefined);
    const pinned = bodies.filter(query => query.getObserversCount() > 0 || query.state.fetchStatus !== 'idle');
    const size = (value: unknown) => new TextEncoder().encode(JSON.stringify(value)).length;
    const records = queries.flatMap(query => query.meta?.documentBody === true ? [] :
      ((query.state.data as { pages?: { kind?: string }[] })?.pages ?? []));
    const scroll = document.querySelector<HTMLElement>('[data-testid="document-scroll"]')!;
    const pane = /(auto|scroll)/.test(getComputedStyle(scroll).overflowY);
    const viewport = pane ? scroll.getBoundingClientRect() : { top: 0, bottom: window.innerHeight };
    return { mountedUnits: scroll.querySelectorAll('[data-visible-unit]').length,
      visibleUnits: [...scroll.querySelectorAll('[data-visible-unit]')].filter(unit => {
        const bounds = unit.getBoundingClientRect();
        return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
      }).length,
      documentNodes: scroll.querySelectorAll('*').length,
      retainedBodies: bodies.length, serializedBodyBytes: bodies.reduce((total, query) =>
        total + size(query.state.data), 0),
      pinnedBodies: pinned.length, pinnedBodyBytes: pinned.reduce((total, query) => total + size(query.state.data), 0),
      mainRecords: records.filter(record => record.kind === 'units').length,
      foldRecords: records.filter(record => record.kind === 'fold').length,
      findOpen: Boolean(document.querySelector('.document-find-panel')),
      revealedFind: Boolean(document.querySelector('.document-find-source mark')),
      records: queries.reduce((total, query) => total + ((query.state.data as { pages?: unknown[] })?.pages?.length ?? 0), 0),
      errors: queries.filter(query => query.state.status === 'error').map(query => query.state.error?.message),
      scrollRoot: pane ? 'pane' : 'document', scrollTop: pane ? scroll.scrollTop : window.scrollY,
      visibilityState: document.visibilityState };
  });
  await session.send('HeapProfiler.collectGarbage');
  return { ...state, heap: await session.send('Runtime.getHeapUsage'), dom: await session.send('Memory.getDOMCounters') };
}

async function frameSweep(page: Page, scrolling: boolean, direction = 1, activeBegin?: number) {
  return page.evaluate(async ({ frames, scrolling, direction, activeWorkload, activeBegin, fragmentBytes }) => {
    const scroll = document.querySelector<HTMLElement>('[data-testid="document-scroll"]')!;
    const intervals: number[] = [];
    const longFrames: { startTime: number; duration: number }[] = [];
    let blankFrames = 0;
    let blankRun = 0;
    let maximumBlankRun = 0;
    let conversationFrames = 0;
    let findOnlyFrames = 0;
    let exposedConversationFrames = 0;
    let rawDetailFrames = 0;
    let toolFragmentFrames = 0;
    let preparedSourceFrames = 0;
    const pane = /(auto|scroll)/.test(getComputedStyle(scroll).overflowY);
    const scroller = pane ? scroll : document.scrollingElement!;
    const viewport = pane ? scroll.getBoundingClientRect() : { top: 0, left: 0, width: window.innerWidth, height: window.innerHeight };
    const sampleX = viewport.left + Math.min(100, viewport.width / 2);
    const sampleYs = [0.25, 0.5, 0.75].map(fraction => viewport.top + viewport.height * fraction);
    const panel = activeWorkload ? document.querySelector('.document-find-panel')?.getBoundingClientRect() : undefined;
    const exposedTop = Math.max(viewport.top, panel?.bottom ?? viewport.top);
    const exposedBottom = viewport.top + viewport.height - 8;
    const exposedSampleYs = panel && exposedBottom > exposedTop
      ? [0.25, 0.5, 0.75].map(fraction => exposedTop + (exposedBottom - exposedTop) * fraction) : [];
    const supported = PerformanceObserver.supportedEntryTypes.includes('long-animation-frame');
    const started = performance.now();
    const observer = supported ? new PerformanceObserver(list => {
      for (const entry of list.getEntries()) longFrames.push({ startTime: entry.startTime, duration: entry.duration });
    }) : undefined;
    observer?.observe({ type: 'long-animation-frame' });
    let previous: number | undefined;
    await new Promise<void>(resolve => {
      const advance = (timestamp: number) => {
        if (previous !== undefined) intervals.push(timestamp - previous);
        previous = timestamp;
        if (scrolling) {
          const exposedHits = exposedSampleYs.map(sampleY => document.elementFromPoint(sampleX, sampleY));
          const hits = [...sampleYs.map(sampleY => document.elementFromPoint(sampleX, sampleY)), ...exposedHits];
          const conversation = hits.some(element => element?.closest('[data-visible-unit]'));
          const found = activeWorkload && hits.some(element => element?.closest('.document-find-preview, .document-find-source'));
          const visible = conversation || found;
          if (conversation) conversationFrames++;
          else if (found) findOnlyFrames++;
          if (exposedHits.some(element => element?.closest('[data-visible-unit]'))) exposedConversationFrames++;
          if (activeBegin !== undefined) {
            const unit = scroll.querySelector(`[data-visible-unit="${activeBegin}"]`);
            const raw = unit?.querySelector(`details[open][data-deferred-event="${activeBegin + 1}"] details[open] pre`);
            const fragment = unit?.querySelector(`details[open][data-deferred-event="${activeBegin + 2}"] [data-fragment-offset="${fragmentBytes}"] pre`);
            if (raw?.textContent?.startsWith(`Synthetic raw result ${activeBegin + 1}: `)) rawDetailFrames++;
            if (fragment?.textContent?.startsWith(`Synthetic fragment ${activeBegin + 2}/${fragmentBytes}: `)) toolFragmentFrames++;
            if (document.querySelector('.document-find-source mark') && document.querySelector('.document-find-preview mark')) preparedSourceFrames++;
          }
          blankRun = visible ? 0 : blankRun + 1;
          if (!visible) blankFrames++;
          maximumBlankRun = Math.max(maximumBlankRun, blankRun);
        }
        if (scrolling && intervals.length < frames) scroller.scrollTop += direction * (intervals.length < frames / 2 ? 64 : -64);
        if (intervals.length >= frames) resolve();
        else requestAnimationFrame(advance);
      };
      requestAnimationFrame(advance);
    });
    await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
    for (const entry of observer?.takeRecords() ?? []) longFrames.push({ startTime: entry.startTime, duration: entry.duration });
    observer?.disconnect();
    const ordered = [...intervals].sort((left, right) => left - right);
    return { intervals, medianMs: ordered[Math.floor(ordered.length / 2)],
      p95Ms: ordered[Math.ceil(ordered.length * 0.95) - 1], maxMs: ordered.at(-1)!,
      over50ms: intervals.filter(interval => interval > 50).length,
      longAnimationFramesSupported: supported, longFrames: longFrames.filter(entry => entry.startTime >= started),
      blankFrames, maximumBlankRun, conversationFrames, findOnlyFrames, exposedConversationFrames,
      exposedProbe: { x: sampleX, ys: exposedSampleYs, top: exposedTop, bottom: exposedBottom, panelBottom: panel?.bottom },
      rawDetailFrames, toolFragmentFrames, preparedSourceFrames,
      sampledFrames: conversationFrames + findOnlyFrames + blankFrames, elapsedMs: performance.now() - started };
  }, { frames, scrolling, direction, activeWorkload, activeBegin, fragmentBytes });
}

async function scrollPosition(page: Page) {
  return page.getByTestId('document-scroll').evaluate(element =>
    /(auto|scroll)/.test(getComputedStyle(element).overflowY) ? element.scrollTop : window.scrollY);
}

async function openActiveWork(page: Page) {
  const started = Date.now();
  await page.getByRole('button', { name: 'Pause document readers', exact: true }).focus();
  const begin = await page.getByTestId('document-scroll').evaluate(element => {
    const viewport = /(auto|scroll)/.test(getComputedStyle(element).overflowY)
      ? element.getBoundingClientRect() : { top: 0, bottom: window.innerHeight };
    const unit = [...element.querySelectorAll<HTMLElement>('[data-visible-unit]')].find(unit => {
      const bounds = unit.getBoundingClientRect();
      return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
    });
    return unit ? Number(unit.dataset.visibleUnit) : -1;
  });
  expect(begin).toBeGreaterThanOrEqual(0);
  const unit = page.locator(`[data-visible-unit="${begin}"]`);
  const fold = unit.locator(':scope > button');
  if (await fold.getAttribute('aria-expanded') !== 'true') await fold.click();
  await expect(unit.locator('[data-document-fold]')).toHaveAttribute('aria-busy', 'false');
  const foldBeforeNavigation = await unit.locator('[data-document-fold]').evaluate(element => ({
    top: element.getBoundingClientRect().top, bottom: element.getBoundingClientRect().bottom,
    mounted: element.querySelector('[data-document-page]')?.getAttribute('data-page-mounted'),
    deferredEvents: element.querySelectorAll('[data-deferred-event]').length,
  }));
  await unit.locator('[data-document-fold]').evaluate(element => element.scrollIntoView({ block: 'start' }));
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(29);
  const raw = unit.locator(`[data-deferred-event="${begin + 1}"]`);
  if (!await raw.evaluate(element => element.hasAttribute('open'))) await raw.locator(':scope > summary').click();
  const detail = raw.locator('details');
  if (!await detail.evaluate(element => element.hasAttribute('open'))) await detail.locator(':scope > summary').click();
  await expect(raw.locator('pre')).toHaveText(rawResult(begin + 1));
  const fragment = unit.locator(`[data-deferred-event="${begin + 2}"]`);
  if (!await fragment.evaluate(element => element.hasAttribute('open'))) await fragment.locator(':scope > summary').click();
  if (await fragment.locator('[data-fragment-offset]').getAttribute('data-fragment-offset') !== String(fragmentBytes)) {
    await fragment.getByRole('button', { name: 'Next fragment', exact: true }).click();
  }
  await expect(fragment.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await expect(fragment.locator('pre')).toHaveText(fragmentText(begin + 2, fragmentBytes));
  await settle(page);
  const detailsReadyMs = Date.now() - started;
  const beforeSearch = await scrollPosition(page);
  const scrollTrace = { beforeSearch } as Record<string, number>;
  const needle = `Synthetic fragment ${begin + 2}/${fragmentBytes}`;
  const searchStarted = Date.now();
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  scrollTrace.afterOpen = await scrollPosition(page);
  await page.getByRole('combobox', { name: 'Search representation' }).selectOption('display');
  scrollTrace.afterSelect = await scrollPosition(page);
  await page.getByRole('textbox', { name: 'Find text' }).fill(needle);
  scrollTrace.afterInput = await scrollPosition(page);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.locator('.document-find-preview mark')).toHaveText(needle);
  scrollTrace.afterSearch = await scrollPosition(page);
  const findReadyMs = Date.now() - searchStarted;
  const revealStarted = Date.now();
  await page.getByRole('button', { name: 'Read the matching conversation', exact: true }).click();
  await expect(page.locator('.document-find-source')).toHaveText(fragmentText(begin + 2, fragmentBytes));
  await expect(page.locator('.document-find-source mark')).toHaveText(needle);
  scrollTrace.afterReveal = await scrollPosition(page);
  await page.locator('.document-find-panel').evaluate(element => {
    const source = element.querySelector('.document-find-source')!;
    element.scrollTop += source.getBoundingClientRect().top - element.getBoundingClientRect().top - 40;
  });
  await settle(page);
  scrollTrace.afterPanelScroll = await scrollPosition(page);
  for (const [interaction, position] of Object.entries(scrollTrace)) {
    expect(Math.abs(position - beforeSearch), `${interaction}: ${JSON.stringify(scrollTrace)}`).toBeLessThanOrEqual(1);
  }
  return { begin, foldBeforeNavigation, detailsReadyMs, findReadyMs, revealReadyMs: Date.now() - revealStarted, scrollTrace, needle };
}

async function expectActiveWork(page: Page, begin: number, needle: string) {
  const unit = page.locator(`[data-visible-unit="${begin}"]`);
  await expect(unit.locator(':scope > button')).toHaveAttribute('aria-expanded', 'true');
  await expect(unit.locator(`[data-deferred-event="${begin + 1}"] pre`)).toHaveText(rawResult(begin + 1));
  await expect(unit.locator(`[data-deferred-event="${begin + 2}"] [data-fragment-offset]`))
    .toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await expect(page.locator('.document-find-source mark')).toHaveText(needle);
}

async function metrics(session: CDPSession) {
  return Object.fromEntries((await session.send('Performance.getMetrics')).metrics.map((metric: { name: string; value: number }) => [metric.name, metric.value]));
}

test.describe('opt-in production document scrolling cost', () => {
  test.skip(!enabled, 'Explicit local cost run, not normal CI wall-clock gate');
  for (let sample = 1; sample <= (Number.isSafeInteger(samples) ? samples : 0); sample++) {
    for (const width of [1280, 390]) for (const pageCount of [10, 100]) for (const windowed of sample % 2 ? [false, true] : [true, false]) {
      test(`${width}px ${pageCount} pages ${windowed ? 'windowed' : 'baseline'} sample ${sample}${activeWorkload ? ' active' : ''}`, async ({ page }, info) => {
        test.setTimeout(180_000);
        const bundle = variants.get(windowed)!;
        const reads: string[] = [];
        const failures: string[] = [];
        const errors: string[] = [];
        const activeReads: ActiveRead[] = [];
        const phases: object[] = [];
        const session = await page.context().newCDPSession(page);
        page.on('pageerror', error => errors.push(error.message));
        await page.setViewportSize({ width, height: 720 });
        await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
        await page.route('**/*', route => {
          if (new URL(route.request().url()).origin !== 'http://127.0.0.1:4174') {
            failures.push(`Nonlocal request ${route.request().url()}`);
            return route.abort();
          }
          return route.continue();
        });
        await page.route('**/__document-cost*', route => {
          const pathname = new URL(route.request().url()).pathname;
          if (pathname.endsWith('.js')) return route.fulfill({ contentType: 'application/javascript', body: bundle.script });
          if (pathname.endsWith('.css')) return route.fulfill({ contentType: 'text/css', body: bundle.styles });
          return route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1" />
<link rel="stylesheet" href="/__document-cost.css" /></head><body><div id="root"></div><script type="module" src="/__document-cost.js"></script></body></html>` });
        });
        await page.route('**/api/v1/**', route => {
          const url = new URL(route.request().url());
          const respond = (kind: string, index: number, offset: number, expected: Record<string, string>, body: unknown, budget: number) => {
            const serialized = JSON.stringify(body);
            const responseBytes = new TextEncoder().encode(serialized).length;
            if (route.request().method() !== 'GET' || url.searchParams.size !== Object.keys(expected).length ||
              Object.entries(expected).some(([key, value]) => url.searchParams.get(key) !== value) || responseBytes > budget) {
              failures.push(`Unexpected active read ${url} (${responseBytes} bytes)`);
              return route.fulfill({ status: 400, json: { error: { code: 'fixture_unexpected_read', message: 'Unexpected active cost read' } } });
            }
            activeReads.push({ kind, index, offset, responseBytes, parameters: Object.fromEntries(url.searchParams) });
            return route.fulfill({ contentType: 'application/json', body: serialized });
          };
          if (activeWorkload) {
            const index = Number(url.searchParams.get('index'));
            const offset = Number(url.searchParams.get('offset'));
            if (url.pathname === '/api/v1/repos/repo/docs/document/event-fragments' && Number.isSafeInteger(index) &&
              index >= 0 && index < pageCount * 620 && index % 31 === 2 && [0, fragmentBytes].includes(offset)) {
              return respond('fragment', index, offset, { index: String(index), offset: String(offset), budget: String(DOCUMENT_FRAGMENT_BYTES) },
                { event_index: index, byte_offset: offset, total_bytes: fragmentTotalBytes,
                  next: offset + fragmentBytes, json_fragment: fragmentText(index, offset) }, DOCUMENT_FRAGMENT_RESPONSE_BYTES);
            }
            const expand = url.searchParams.get('expand');
            if (url.pathname === '/api/v1/repos/repo/docs/document/events' && expand && /^fold-\d+$/.test(expand)) {
              const begin = Number(expand.slice(5));
              if (begin >= 0 && begin < pageCount * 620 && begin % 31 === 0)
                return respond('fold', begin, 0, { projection: 'visible-v1', expand }, memoryFold(begin, pageCount), DOCUMENT_PAGE_BYTES);
            }
            if (url.pathname === '/api/v1/repos/repo/docs/document/events' && url.searchParams.get('view') === 'raw') {
              const event = Number(url.searchParams.get('offset'));
              if (Number.isSafeInteger(event) && event >= 0 && event < pageCount * 620 && event % 31 === 1)
                return respond('event', event, 0, { view: 'raw', offset: String(event), limit: '1' },
                  { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' }, offset: event, total: pageCount * 620,
                    inherited: 0, next: event + 1, events: [{ kind: 'tool_result', event_index: event, deferred: false, output: rawResult(event) }] }, DOCUMENT_DETAIL_BYTES);
            }
            if (url.pathname === '/api/v1/repos/repo/docs/document/find') {
              const matched = /^Synthetic fragment (\d+)\/4096$/.exec(url.searchParams.get('q') ?? '');
              const event = matched ? Number(matched[1]) : -1;
              if (event >= 0 && event < pageCount * 620 && event % 31 === 2) {
                const needle = `Synthetic fragment ${event}/${fragmentBytes}`;
                return respond('find', event, fragmentBytes,
                  { q: needle, selection: 'main-0', from: '0', to: String(pageCount * 620), representation: 'display-prepared-v1' },
                  { projection: 'find-display-prepared-v1', hash: 'document', generation: 'generation-one', from: 0, to: pageCount * 620,
                    event_index: pageCount * 620, field: 0, byte_offset: 0, prepared: { page: 0, generation: '' }, next_cursor: null,
                    matches: [{ event_index: event, field: 0, byte_offset: fragmentBytes, byte_length: needle.length, raw: false,
                      preview: fragmentText(event, fragmentBytes).slice(0, needle.length + 32), preview_byte_offset: fragmentBytes,
                      reveal_cursor: `reveal-${event}`, prepared_page: 0, source_range: { from: fragmentBytes, to: fragmentBytes + needle.length } }] }, DOCUMENT_PAGE_BYTES);
              }
            }
            if (url.pathname === '/api/v1/repos/repo/docs/document/find-fragments') {
              const matched = /^reveal-(\d+)$/.exec(url.searchParams.get('selection') ?? '');
              const event = matched ? Number(matched[1]) : -1;
              if (event >= 0 && event < pageCount * 620 && event % 31 === 2)
                return respond('find-fragment', event, fragmentBytes,
                  { selection: `reveal-${event}`, offset: String(fragmentBytes), budget: String(DOCUMENT_FRAGMENT_BYTES) },
                  { projection: 'find-fragment-v1', hash: 'document', generation: 'generation-one', event_index: event,
                    byte_offset: fragmentBytes, total_bytes: fragmentTotalBytes, next: 2 * fragmentBytes, previous: 0,
                    json_fragment: fragmentText(event, fragmentBytes) }, DOCUMENT_FRAGMENT_RESPONSE_BYTES);
            }
          }
          const cursor = url.searchParams.get('cursor');
          const from = cursor ? Number(cursor.replace('main-', '')) : 0;
          const expected: Record<string, string> = cursor === null
            ? { projection: 'visible-v1', view: 'chat', offset: '-1', limit: '20', base: 'base' }
            : { projection: 'visible-v1', cursor };
          if (route.request().method() !== 'GET' || url.pathname !== '/api/v1/repos/repo/docs/document/events' ||
            url.searchParams.size !== Object.keys(expected).length ||
            Object.entries(expected).some(([key, value]) => url.searchParams.get(key) !== value) ||
            cursor !== null && !/^main-\d+$/.test(cursor) || !Number.isSafeInteger(from) || from < 0 ||
            from % 620 !== 0 || from >= pageCount * 620) {
            failures.push(`Unexpected read ${url}`);
            return route.fulfill({ status: 400, json: { error: { code: 'fixture_unexpected_read', message: 'Unexpected cost read' } } });
          }
          reads.push(cursor ?? 'initial');
          return route.fulfill({ json: heavyTextVisiblePage(from, pageCount) });
        });
        let outcome = 'incomplete';
        try {
          await page.goto(activeWorkload ? '/__document-cost?active=1' : '/__document-cost');
          await expect(page.getByRole('button', { name: 'Resume document readers', exact: true })).toBeVisible();
          await session.send('Performance.enable');
          const idle = await frameSweep(page, false);
          await page.getByRole('button', { name: 'Resume document readers', exact: true }).click();
          const scroll = page.getByTestId('document-scroll');
          for (let visited = 1; visited <= pageCount; visited++) {
            await expect.poll(async () => {
              const target = page.locator(`[data-visible-unit="${visited * 620 - 31}"]`);
              if (await target.count()) return 1;
              await scroll.evaluate(element => {
                if (/(auto|scroll)/.test(getComputedStyle(element).overflowY)) element.scrollTop = element.scrollHeight;
                else window.scrollTo(0, document.documentElement.scrollHeight);
              });
              return target.count();
            }, { timeout: 20_000 }).toBe(1);
          }
          await expect(page.locator('[data-document-sentinel]')).toHaveCount(0);
          await settle(page);
          expect(reads).toHaveLength(pageCount);
          phases.push({ name: 'forward traversal', state: await snapshot(page, session) });
          for (const position of [0, 0.5, 1]) {
            if (activeWorkload && await page.locator('.document-find-panel').count())
              await page.getByRole('button', { name: 'Find in document', exact: true }).click();
            await scroll.evaluate((element, fraction) => {
              const pane = /(auto|scroll)/.test(getComputedStyle(element).overflowY);
              const scroller = pane ? element : document.scrollingElement!;
              scroller.scrollTop = (scroller.scrollHeight - (pane ? element.clientHeight : window.innerHeight)) * fraction;
            }, position);
            await expect.poll(() => scroll.locator('[data-visible-unit]').count()).toBeGreaterThan(0);
            await settle(page);
            await expect.poll(() => scroll.evaluate(element => {
              const viewport = /(auto|scroll)/.test(getComputedStyle(element).overflowY)
                ? element.getBoundingClientRect() : { top: 0, bottom: window.innerHeight };
              return [...element.querySelectorAll('[data-visible-unit]')].filter(unit => {
                const bounds = unit.getBoundingClientRect();
                return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
              }).length;
            })).toBeGreaterThan(0);
            await settle(page);
            const work = activeWorkload ? await openActiveWork(page) : null;
            if (work) await expectActiveWork(page, work.begin, work.needle);
            const before = await metrics(session);
            const timing = await frameSweep(page, true, position === 1 ? -1 : 1, work?.begin);
            const after = await metrics(session);
            await settle(page);
            if (work) await expectActiveWork(page, work.begin, work.needle);
            const state = await snapshot(page, session);
            phases.push({ name: `scroll from ${position}`, work, idle, timing, state,
              performanceSeconds: Object.fromEntries(['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration'].map(name =>
                [name, after[name] - before[name]])) });
            expect(timing.intervals).toHaveLength(frames);
            expect(state.errors).toEqual([]);
            expect(state.visibleUnits).toBeGreaterThan(0);
            expect(state.scrollRoot).toBe(width > 1000 ? 'pane' : 'document');
            expect(state.visibilityState).toBe('visible');
            expect(timing.blankFrames).toBeLessThan(frames);
            expect(activeWorkload ? state.mainRecords : state.records).toBe(pageCount);
            if (activeWorkload) {
              expect(timing.blankFrames).toBe(0);
              expect(timing.exposedConversationFrames).toBe(timing.sampledFrames);
              expect(timing.rawDetailFrames).toBeGreaterThan(0);
              expect(timing.toolFragmentFrames).toBeGreaterThan(0);
              expect(timing.preparedSourceFrames).toBe(timing.sampledFrames);
              expect(state.findOpen && state.revealedFind).toBe(true);
            }
            if (windowed) {
              expect(state.mountedUnits).toBeLessThanOrEqual(60);
              expect(state.retainedBodies).toBeLessThanOrEqual(activeWorkload ? Math.max(8, state.pinnedBodies) : 8);
              expect(state.serializedBodyBytes).toBeLessThanOrEqual(activeWorkload ? Math.max(8 << 20, state.pinnedBodyBytes) : 8 << 20);
              expect(timing.p95Ms).toBeLessThanOrEqual(Math.max(50, idle.p95Ms * 2));
            }
          }
          if (activeWorkload) {
            expect(activeReads.filter(read => read.kind === 'find')).toHaveLength(3);
            expect(activeReads.filter(read => read.kind === 'find-fragment')).toHaveLength(3);
            expect(activeReads.filter(read => read.kind === 'fold').length).toBeGreaterThanOrEqual(3);
            await page.getByRole('button', { name: 'Pause document readers', exact: true }).click();
            await settle(page);
            const unpinned = await snapshot(page, session);
            phases.push({ name: 'readers and search unmounted', state: unpinned });
            expect(unpinned.mountedUnits).toBe(0);
            expect(unpinned.findOpen).toBe(false);
            expect(unpinned.pinnedBodies).toBe(0);
            expect(unpinned.retainedBodies).toBeLessThanOrEqual(8);
            expect(unpinned.serializedBodyBytes).toBeLessThanOrEqual(8 << 20);
          }
          expect(reads.filter(read => read === 'initial')).toHaveLength(1);
          expect(failures).toEqual([]);
          expect(errors).toEqual([]);
          outcome = 'fixed-workload assertions passed';
        } finally {
          const report = { method: activeWorkload ? 4 : 2, activeWorkload, sample, pageCount, windowed, outcome, productionReact: true, minified: true,
            browserVersion: page.context().browser()?.version(),
            fixture: `20 anchors/620 raw events and 90KiB distinct anchor text per page, ${activeWorkload ? 'active fold/raw/fragment and prepared find/reveal' : 'closed folds'}`,
            framesPerSweep: frames, viewport: page.viewportSize(), phases, reads, activeReads, failures, errors,
            limitations: ['Local synthetic loopback reads, not cloud/API/database latency or field INP.',
              'rAF intervals include scheduling; CDP duration deltas are not isolated React CPU.',
              'Three viewport hit tests, plus three active-only exposed-strip probes, contribute equal per-variant measurement overhead.',
              '50ms is a local frame diagnostic floor, not a production SLO; same-context idle p95 normalizes slower displays.',
              'Active search overlays count as visible content separately from conversation hits; timing excludes interaction setup and GC.',
              'Each 120-interval sweep has 121 viewport samples; active disclosure presence is recorded, and offscreen unmount/restoration is allowed.',
              'Synthetic search/source fragments measure the real UI protocol, not backend search quality or canonical source mapping.',
              'Lightweight metadata/placeholders remain proportional to visited pages; serialized cache bytes are not heap bytes.'] };
          console.log(JSON.stringify(report));
          await info.attach('document-cost', { body: JSON.stringify(report, null, 2), contentType: 'application/json' });
          await session.detach().catch(() => undefined);
        }
      });
    }
  }
});
