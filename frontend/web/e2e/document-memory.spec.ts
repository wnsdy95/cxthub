import { test, expect, type CDPSession, type Page, type Route } from '@playwright/test';
import type { QueryClient } from '@tanstack/react-query';
import { heavyTextVisiblePage } from '../tests/documentVisibleFixture';
import { fragmentBytes, fragmentTotalBytes, memoryFold, rawResult, fragmentText } from '../tests/documentActiveFixture';
import type { DocVisiblePage } from '../src/documentVisible';
import { DOCUMENT_DETAIL_BYTES, DOCUMENT_FRAGMENT_BYTES, DOCUMENT_FRAGMENT_RESPONSE_BYTES,
  DOCUMENT_PAGE_BYTES, DOCUMENT_RAW_EVENT_BYTES } from '../src/documentReads';

const enabled = process.env.CXT_DOCUMENT_MEMORY === '1' && process.env.CXT_DOCUMENT_WINDOW === '1';
test.use({ trace: 'off', video: 'off', screenshot: 'off' });
const rawEventsPerPage = 620;
const anchorsPerPage = 20;
const anchorTextBytes = 4608;
const fragmentScrollTop = 500;
const samplesPerPhase = 3;
const bodyTarget = { entries: 8, serializedBytes: 8 * 1024 * 1024 };

type RequestKind = 'units' | 'fold' | 'event' | 'fragment';
type ReadEvidence = { kind: RequestKind; selector: string | null; index: number; offset: number; responseBytes: number };
type BodyEvidence = { kind: string; key: readonly unknown[]; from: number | null; eventIndex: number | null;
  serializedBytes: number; observers: number; fetchStatus: string };
type RetentionEvidence = {
  heapIndependentSerialization: true;
  retainedBodies: number;
  retainedSerializedBodyBytes: number;
  bodies: BodyEvidence[];
  activeBodyExceptions: BodyEvidence[];
  records: { units: number; fold: number; other: number; serializedBytes: number; inlineBodies: number };
  queries: number;
  fetching: number;
  queryErrors: string[];
  unscopedDetailBodies: number;
  bodyOwners: string[];
  mountedUnits: number;
  mountedPages: number;
  pagePlaceholders: number;
  documentElements: number;
  scrollTop: number;
};
type MemorySample = {
  sample: number;
  elapsedMs: number;
  heapUsedBytes: number;
  heapTotalBytes: number;
  embedderHeapUsedBytes: number | null;
  backingStorageBytes: number | null;
  dom: { documents: number; nodes: number; jsEventListeners: number };
  retention: RetentionEvidence;
};
type MemoryPhase = {
  name: string;
  cacheTargetGate: 'pre-load' | 'active-pins-accepted-not-a-hard-cap' | 'quiescent-unpinned';
  samples: MemorySample[];
  medians: { heapUsedBytes: number; heapTotalBytes: number; domNodes: number; domDocuments: number;
    jsEventListeners: number; retainedSerializedBodyBytes: number; retainedBodies: number };
  deltaFromBaseline: Record<string, number> | null;
  deltaFromPreviousPhase: Record<string, number> | null;
};

function memoryPage(from: number, pageCount: number): DocVisiblePage {
  return heavyTextVisiblePage(from, pageCount);
}

async function retention(page: Page): Promise<RetentionEvidence> {
  return page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient;
    const queries = client.getQueryCache().getAll();
    const encoder = new TextEncoder();
    const size = (value: unknown) => value === undefined ? 0 : encoder.encode(JSON.stringify(value)).byteLength;
    const bodies = queries.filter(query => query.meta?.documentBody === true).map(query => {
      const data = query.state.data as { from?: number; event_index?: number } | undefined;
      return { kind: String(query.queryKey[8] ?? 'unknown'), key: query.queryKey,
        from: data?.from ?? null, eventIndex: data?.event_index ?? null,
        serializedBytes: size(data), observers: query.getObserversCount(), fetchStatus: query.state.fetchStatus };
    });
    const retained = bodies.filter(body => body.serializedBytes > 0);
    const records = { units: 0, fold: 0, other: 0, serializedBytes: 0, inlineBodies: 0 };
    for (const query of queries) {
      if (query.meta?.documentBody === true) continue;
      const data = query.state.data as { pages?: { kind?: string; initial?: unknown; units?: unknown; events?: unknown }[] } | undefined;
      if (!Array.isArray(data?.pages)) continue;
      records.serializedBytes += size(data);
      for (const record of data.pages) {
        if (record.kind === 'units') records.units++;
        else if (record.kind === 'fold') records.fold++;
        else records.other++;
        if (record.initial !== undefined || record.units !== undefined || record.events !== undefined) records.inlineBodies++;
      }
    }
    const scroll = document.querySelector<HTMLElement>('[data-testid="document-scroll"]')!;
    return { heapIndependentSerialization: true, retainedBodies: retained.length,
      retainedSerializedBodyBytes: retained.reduce((total, body) => total + body.serializedBytes, 0), bodies: retained,
      activeBodyExceptions: bodies.filter(body => body.observers > 0 || body.fetchStatus !== 'idle'), records,
      queries: queries.length, fetching: client.isFetching(),
      queryErrors: queries.filter(query => query.state.status === 'error').map(query => query.state.error?.message ?? 'Unknown query error'),
      unscopedDetailBodies: queries.filter(query => ['doc-event-detail', 'doc-event-fragment'].includes(String(query.queryKey[0])) && query.state.data !== undefined).length,
      bodyOwners: [...new Set(retained.map(body => JSON.stringify(body.key.slice(0, 7))))],
      mountedUnits: scroll.querySelectorAll('[data-visible-unit]').length,
      mountedPages: scroll.querySelectorAll('[data-document-page][data-page-mounted="true"]').length,
      pagePlaceholders: scroll.querySelectorAll('[data-document-page][data-page-mounted="false"]').length,
      documentElements: scroll.querySelectorAll('*').length, scrollTop: scroll.scrollTop };
  });
}

async function settle(page: Page) {
  await expect.poll(() => page.evaluate(() =>
    (window as unknown as { documentQueryClient: QueryClient }).documentQueryClient.isFetching()), { timeout: 20_000 }).toBe(0);
  await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  await page.waitForTimeout(200);
}

function median(values: number[]): number {
  return [...values].sort((left, right) => left - right)[Math.floor(values.length / 2)];
}

function deltas(current: MemoryPhase['medians'], previous: MemoryPhase['medians']): Record<string, number> {
  return Object.fromEntries(Object.entries(current).map(([name, value]) => [name, value - previous[name as keyof typeof previous]]));
}

async function capturePhase(page: Page, session: CDPSession, phases: MemoryPhase[], name: string,
  cacheTargetGate: MemoryPhase['cacheTargetGate'], started: number) {
  const phase: MemoryPhase = { name, cacheTargetGate, samples: [], medians: {
    heapUsedBytes: 0, heapTotalBytes: 0, domNodes: 0, domDocuments: 0, jsEventListeners: 0,
    retainedSerializedBodyBytes: 0, retainedBodies: 0 }, deltaFromBaseline: null, deltaFromPreviousPhase: null };
  for (let sampleIndex = 0; sampleIndex < samplesPerPhase; sampleIndex++) {
    await settle(page);
    const observed = await retention(page);
    await session.send('HeapProfiler.collectGarbage');
    const heap = await session.send('Runtime.getHeapUsage');
    const dom = await session.send('Memory.getDOMCounters');
    phase.samples.push({ sample: sampleIndex + 1, elapsedMs: Date.now() - started,
      heapUsedBytes: heap.usedSize, heapTotalBytes: heap.totalSize,
      embedderHeapUsedBytes: heap.embedderHeapUsedSize ?? null, backingStorageBytes: heap.backingStorageSize ?? null,
      dom, retention: observed });
  }
  phase.medians = {
    heapUsedBytes: median(phase.samples.map(sample => sample.heapUsedBytes)),
    heapTotalBytes: median(phase.samples.map(sample => sample.heapTotalBytes)),
    domNodes: median(phase.samples.map(sample => sample.dom.nodes)),
    domDocuments: median(phase.samples.map(sample => sample.dom.documents)),
    jsEventListeners: median(phase.samples.map(sample => sample.dom.jsEventListeners)),
    retainedSerializedBodyBytes: median(phase.samples.map(sample => sample.retention.retainedSerializedBodyBytes)),
    retainedBodies: median(phase.samples.map(sample => sample.retention.retainedBodies)),
  };
  if (phases.length) {
    phase.deltaFromBaseline = deltas(phase.medians, phases[0].medians);
    phase.deltaFromPreviousPhase = deltas(phase.medians, phases.at(-1)!.medians);
  }
  phases.push(phase);
  for (const sample of phase.samples) {
    expect(sample.retention.fetching).toBe(0);
    expect(sample.retention.queryErrors).toEqual([]);
    expect(sample.retention.records.inlineBodies).toBe(0);
    expect(sample.retention.unscopedDetailBodies).toBe(0);
    if (cacheTargetGate === 'quiescent-unpinned') {
      expect(sample.retention.activeBodyExceptions).toEqual([]);
      expect(sample.retention.retainedBodies).toBeLessThanOrEqual(bodyTarget.entries);
      expect(sample.retention.retainedSerializedBodyBytes).toBeLessThanOrEqual(bodyTarget.serializedBytes);
    }
  }
  return phase;
}

async function installHarness(page: Page, pageCount: number, origin: string, reads: ReadEvidence[], violations: string[], errors: string[]) {
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/*', route => {
    if (new URL(route.request().url()).origin === origin) return route.continue();
    violations.push(`Nonlocal request blocked: ${route.request().url()}`);
    return route.abort('blockedbyclient');
  });
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    const fulfill = async (kind: RequestKind, body: unknown, expected: Record<string, string>, index = -1, offset = 0) => {
      const actual = Object.fromEntries(url.searchParams);
      if (route.request().method() !== 'GET' || url.origin !== origin || url.searchParams.size !== Object.keys(expected).length ||
        Object.entries(expected).some(([name, value]) => actual[name] !== value)) violations.push(`Unexpected ${kind} request: ${url}`);
      const response = JSON.stringify(body);
      const responseBytes = Buffer.byteLength(response);
      const limit = kind === 'fragment' ? DOCUMENT_FRAGMENT_RESPONSE_BYTES : kind === 'event' ? DOCUMENT_DETAIL_BYTES : DOCUMENT_PAGE_BYTES;
      if (responseBytes > limit) violations.push(`${kind} response exceeds ${limit} bytes: ${responseBytes}`);
      reads.push({ kind, selector: url.searchParams.get('expand') ?? url.searchParams.get('cursor'), index, offset, responseBytes });
      return route.fulfill({ contentType: 'application/json', body: response });
    };
    if (url.pathname === '/api/v1/repos/repo/docs/document/event-fragments') {
      const index = Number(url.searchParams.get('index'));
      const offset = Number(url.searchParams.get('offset'));
      if (Number.isSafeInteger(index) && index >= 0 && index < pageCount * rawEventsPerPage && index % rawEventsPerPage === 2 &&
        Number.isSafeInteger(offset) && offset >= 0 && offset < fragmentTotalBytes && offset % fragmentBytes === 0) {
        return fulfill('fragment', { event_index: index, byte_offset: offset, total_bytes: fragmentTotalBytes,
          next: offset + fragmentBytes === fragmentTotalBytes ? -1 : offset + fragmentBytes, json_fragment: fragmentText(index, offset) },
        { index: String(index), offset: String(offset), budget: String(DOCUMENT_FRAGMENT_BYTES) }, index, offset);
      }
    } else if (url.pathname === '/api/v1/repos/repo/docs/document/events') {
      const expand = url.searchParams.get('expand');
      if (expand && /^fold-\d+$/.test(expand)) {
        const begin = Number(expand.slice('fold-'.length));
        if (begin >= 0 && begin < pageCount * rawEventsPerPage && begin % rawEventsPerPage === 0)
          return fulfill('fold', memoryFold(begin, pageCount), { projection: 'visible-v1', expand }, begin);
      } else if (url.searchParams.get('view') === 'raw') {
        const index = Number(url.searchParams.get('offset'));
        if (Number.isSafeInteger(index) && index >= 0 && index < pageCount * rawEventsPerPage && index % rawEventsPerPage === 1) {
          return fulfill('event', { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' },
            offset: index, total: pageCount * rawEventsPerPage, inherited: 0, next: index + 1,
            events: [{ kind: 'tool_result', event_index: index, deferred: false, output: rawResult(index) }] },
          { view: 'raw', offset: String(index), limit: '1' }, index);
        }
      } else {
        const cursor = url.searchParams.get('cursor');
        const from = cursor && /^main-\d+$/.test(cursor) ? Number(cursor.slice('main-'.length)) : cursor === null ? 0 : -1;
        if (from >= 0 && from < pageCount * rawEventsPerPage && from % rawEventsPerPage === 0)
          return fulfill('units', memoryPage(from, pageCount), cursor === null
            ? { projection: 'visible-v1', view: 'chat', offset: '-1', limit: String(anchorsPerPage), base: 'base' }
            : { projection: 'visible-v1', cursor }, from);
      }
    }
    violations.push(`Unexpected API request blocked: ${url}`);
    return rejectRead(route);
  });
  await page.route('**/__document-memory-harness', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-memory-harness.ts');
</script></body></html>` }));
  await page.goto('/__document-memory-harness');
  await expect(page.getByRole('button', { name: 'Resume document readers', exact: true })).toBeVisible();
}

function rejectRead(route: Route) {
  return route.fulfill({ status: 400, json: { error: { code: 'memory_fixture_unexpected_read', message: 'Memory qualification blocks unexpected reads' } } });
}

async function openWork(page: Page, begin: number) {
  await page.getByRole('button', { name: 'Pause document readers', exact: true }).focus();
  const boundary = page.locator(`[data-document-page="units:${begin}"]`);
  await boundary.evaluate(element => element.scrollIntoView({ block: 'start' }));
  const unit = page.locator(`[data-visible-unit="${begin}"]`);
  await expect(unit).toBeAttached();
  await unit.locator(':scope > button').click();
  await expect(unit.locator('[data-document-fold]')).toHaveAttribute('aria-busy', 'false');
  await expect(unit.locator('[data-deferred-event]')).toHaveCount(29);
  const raw = unit.locator(`[data-deferred-event="${begin + 1}"]`);
  await raw.locator(':scope > summary').click();
  await raw.locator('details').locator(':scope > summary').click();
  await expect(raw.locator('pre')).toHaveText(rawResult(begin + 1));
  const fragment = unit.locator(`[data-deferred-event="${begin + 2}"]`);
  await fragment.locator(':scope > summary').click();
  await fragment.getByRole('button', { name: 'Next fragment', exact: true }).click();
  await expect(fragment.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await expect(fragment.locator('pre')).toHaveText(fragmentText(begin + 2, fragmentBytes));
  await fragment.locator('pre').evaluate((element, top) => { element.scrollTop = top; }, fragmentScrollTop);
  await expect.poll(() => fragment.locator('pre').evaluate(element => element.scrollTop)).toBe(fragmentScrollTop);
  await settle(page);
}

async function expectRestoredWork(page: Page) {
  const unit = page.locator('[data-visible-unit="0"]');
  await expect(unit.locator(':scope > button')).toHaveAttribute('aria-expanded', 'true');
  await expect(unit.locator('[data-document-fold]')).toHaveAttribute('aria-busy', 'false');
  const raw = unit.locator('[data-deferred-event="1"]');
  await expect(raw).toHaveAttribute('open', '');
  await expect(raw.locator('details')).toHaveAttribute('open', '');
  await expect(raw.locator('pre')).toHaveText(rawResult(1));
  const fragment = unit.locator('[data-deferred-event="2"]');
  await expect(fragment).toHaveAttribute('open', '');
  await expect(fragment.locator('[data-fragment-offset]')).toHaveAttribute('data-fragment-offset', String(fragmentBytes));
  await expect(fragment.locator('pre')).toHaveText(fragmentText(2, fragmentBytes));
  await expect.poll(() => fragment.locator('pre').evaluate(element => element.scrollTop)).toBe(fragmentScrollTop);
}

test.describe('opt-in document memory qualification evidence', () => {
  test.skip(!enabled, 'Requires both CXT_DOCUMENT_MEMORY=1 and CXT_DOCUMENT_WINDOW=1');

  for (const pageCount of [10, 100]) test(`${pageCount} visited pages: closed folds, active shared bodies and unpinned retention`, async ({ page, browserName, baseURL }, testInfo) => {
    test.skip(browserName !== 'chromium', 'Heap/DOM qualification uses Chromium CDP');
    test.setTimeout(pageCount === 100 ? 600_000 : 180_000);
    expect(baseURL).toBe('http://127.0.0.1:4174');
    expect(fragmentTotalBytes).toBeGreaterThan(DOCUMENT_RAW_EVENT_BYTES);
    const textBytesPerPage = anchorsPerPage * anchorTextBytes;
    expect(textBytesPerPage).toBeGreaterThanOrEqual(80 * 1024);
    expect(textBytesPerPage).toBeLessThanOrEqual(110 * 1024);
    const started = Date.now();
    const reads: ReadEvidence[] = [];
    const violations: string[] = [];
    const errors: string[] = [];
    const phases: MemoryPhase[] = [];
    let sharingEvidence: object | null = null;
    let restorationEvidence: object | null = null;
    let outcome = 'incomplete';
    let failure: string | null = null;
    const session = await page.context().newCDPSession(page);
    try {
      await installHarness(page, pageCount, new URL(baseURL!).origin, reads, violations, errors);
      await session.send('HeapProfiler.enable');
      const baseline = await capturePhase(page, session, phases, 'loaded modules, before document admission', 'pre-load', started);
      expect(reads).toEqual([]);
      expect(baseline.samples.every(sample => sample.retention.records.units === 0 && sample.retention.retainedBodies === 0)).toBe(true);
      await page.getByRole('button', { name: 'Resume document readers', exact: true }).click();
      const scroll = page.getByTestId('document-scroll');
      for (let visited = 1; visited <= pageCount; visited++) {
        const target = page.locator(`[data-visible-unit="${visited * rawEventsPerPage - 31}"]`);
        await expect.poll(async () => {
          if (await target.count()) return 1;
          await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
          return target.count();
        }, { timeout: 20_000 }).toBe(1);
      }
      await page.locator(`[data-document-page="units:${(pageCount - 1) * rawEventsPerPage}"]`).evaluate(element => element.scrollIntoView({ block: 'start' }));
      await expect(page.locator('[data-document-sentinel]')).toHaveCount(0);
      const closed = await capturePhase(page, session, phases, 'all pages visited, folds closed', 'active-pins-accepted-not-a-hard-cap', started);
      expect(reads).toHaveLength(pageCount);
      expect(reads.every(read => read.kind === 'units')).toBe(true);
      for (const sample of closed.samples) {
        expect(sample.retention.records.units).toBe(pageCount);
        expect(sample.retention.records.fold).toBe(0);
        expect(sample.retention.mountedUnits).toBeLessThanOrEqual(60);
        expect(sample.retention.pagePlaceholders).toBeGreaterThan(0);
      }
      for (let pageIndex = 0; pageIndex < pageCount; pageIndex++) await openWork(page, pageIndex * rawEventsPerPage);
      const active = await capturePhase(page, session, phases, 'every visited page opened one fold, raw detail and two tool fragments', 'active-pins-accepted-not-a-hard-cap', started);
      for (const sample of active.samples) {
        expect(sample.retention.records.units).toBe(pageCount);
        expect(sample.retention.records.fold).toBe(pageCount);
        expect(sample.retention.activeBodyExceptions.length).toBeGreaterThan(0);
        expect(sample.retention.bodyOwners).toHaveLength(1);
        for (const kind of ['units', 'fold', 'event', 'fragment']) expect(sample.retention.bodies.some(body => body.kind === kind)).toBe(true);
        expect(sample.retention.mountedUnits).toBeLessThanOrEqual(60);
        expect(sample.retention.bodies.some(body => body.kind === 'units' && body.from === 0)).toBe(false);
      }
      sharingEvidence = { allBodyKindsUseDocumentBodyMetaAndSameOwner: true,
        bodyKinds: ['units', 'fold', 'event', 'fragment'], activePinsAccepted: true,
        observedEntryTargetOverrun: active.samples.some(sample => sample.retention.retainedBodies > bodyTarget.entries),
        observedByteTargetOverrun: active.samples.some(sample => sample.retention.retainedSerializedBodyBytes > bodyTarget.serializedBytes),
        openedPages: pageCount, foldsPerOpenedPage: 1, rawDetailsPerOpenedPage: 1, fragmentsPerOpenedPage: 2 };
      const beforeRestore = reads.length;
      const firstFragmentZeroReads = reads.filter(read => read.kind === 'fragment' && read.index === 2 && read.offset === 0).length;
      await page.getByRole('button', { name: 'Pause document readers', exact: true }).focus();
      await scroll.evaluate(element => { element.scrollTop = 0; });
      await expectRestoredWork(page);
      await expect.poll(() => scroll.evaluate(element => element.scrollTop)).toBe(0);
      await settle(page);
      const restoredReads = reads.slice(beforeRestore);
      expect(restoredReads.filter(read => read.kind === 'units' && read.selector === 'main-0')).toHaveLength(1);
      expect(restoredReads.filter(read => read.kind === 'fold' && read.selector === 'fold-0')).toHaveLength(1);
      expect(restoredReads.filter(read => read.kind === 'event' && read.index === 1)).toHaveLength(1);
      expect(restoredReads.filter(read => read.kind === 'fragment' && read.index === 2 && read.offset === fragmentBytes)).toHaveLength(1);
      expect(reads.filter(read => read.kind === 'fragment' && read.index === 2 && read.offset === 0)).toHaveLength(firstFragmentZeroReads);
      const beforeToggle = reads.length;
      const firstFold = page.locator('[data-visible-unit="0"] > button');
      await firstFold.click();
      await expect(firstFold).toHaveAttribute('aria-expanded', 'false');
      await firstFold.click();
      await expectRestoredWork(page);
      await settle(page);
      expect(reads).toHaveLength(beforeToggle);
      await page.getByRole('button', { name: 'Pause document readers', exact: true }).focus();
      await scroll.evaluate(element => { element.scrollTop = 0; });
      const restored = await capturePhase(page, session, phases, 'backscroll restored evicted bodies and disclosure/fragment state', 'active-pins-accepted-not-a-hard-cap', started);
      restorationEvidence = { firstPageEvictedBeforeBackscroll: true, replayCursor: 'main-0', initialRequests: reads.filter(read => read.kind === 'units' && read.selector === null).length,
        restoredReads, foldOpen: true, rawDetailOpen: true, nestedRawResultOpen: true,
        fragmentOffset: fragmentBytes, fragmentScrollTop, previousFragmentNotReread: true,
        foldCloseReopenExtraRequests: reads.length - beforeToggle, outerScrollTop: restored.samples.at(-1)!.retention.scrollTop };
      await page.getByRole('button', { name: 'Pause document readers', exact: true }).click();
      await expect(page.locator('[data-visible-unit]')).toHaveCount(0);
      await expect.poll(async () => (await retention(page)).activeBodyExceptions.length).toBe(0);
      const unpinned = await capturePhase(page, session, phases, 'readers unmounted, QueryClient retained without clearing or clock acceleration', 'quiescent-unpinned', started);
      for (const sample of unpinned.samples) {
        expect(sample.retention.records.units).toBe(pageCount);
        expect(sample.retention.records.fold).toBe(pageCount);
        expect(sample.retention.mountedPages).toBe(0);
      }
      expect(reads.filter(read => read.kind === 'units' && read.selector === null)).toHaveLength(1);
      expect(violations).toEqual([]);
      expect(errors).toEqual([]);
      outcome = 'workload assertions passed; not R1 completion';
    } catch (error) {
      outcome = 'failed';
      failure = error instanceof Error ? error.message : String(error);
      throw error;
    } finally {
      const report = { qualification: 'partial R1 browser heap/DOM and active-workload evidence, not a release gate completion',
        outcome, failure, visitedPages: pageCount, browser: browserName, viewport: page.viewportSize(),
        protocol: 'visible-v1 with generation-bound self replay for main and fold bodies',
        fixture: { anchorsPerPage, rawEventsPerPage, distinctAnchorTextBytesPerPage: textBytesPerPage,
          rawDetailTextBytes: 4096, fragmentBytes, fragmentTotalBytes, generatedText: 'deterministic per-event pseudorandom ASCII, not repeated shared filler' },
        method: { samplesPerPhase, baseline: 'same warmed harness and modules before document admission',
          collection: ['HeapProfiler.collectGarbage', 'Runtime.getHeapUsage', 'Memory.getDOMCounters'],
          summaries: 'median of three post-GC samples; signed deltas against baseline and previous phase',
          heapSnapshots: false, absoluteTotalHeapAssertion: false, cacheTarget: bodyTarget,
          cacheTargetAppliesOnly: 'quiescent, unpinned readers; active observers/in-flight bodies are accepted exceptions',
          uiPresentationMap: { accessible: false, entries: null, serializedBytes: null, note: 'Private React context; no inferred count or memory bound' } },
        phases, sharingEvidence, restorationEvidence,
        requests: { count: reads.length, byKind: Object.fromEntries((['units', 'fold', 'event', 'fragment'] as const).map(kind => [kind, reads.filter(read => read.kind === kind).length])),
          responseBytes: reads.reduce((total, read) => total + read.responseBytes, 0), reads },
        violations, errors,
        limitations: ['Synthetic local intercepted reads only; no database, production traffic or paid calls.',
          'Three samples per phase are repeated measurements in one fresh context, not independent trial replicates.',
          'Heap/DOM deltas include React, QueryClient, Vite, browser and measurement overhead; serialized body bytes are not retained heap bytes.',
          'Checkpoint/cursor records and page placeholders intentionally grow with visited pages; the private presentation map is unmeasured.',
          'No hard total-heap/DOM ceiling, latency, frame-rate, production performance or R1-complete claim.'] };
      console.log(JSON.stringify(report));
      await testInfo.attach(`document-memory-${pageCount}-pages`, { body: JSON.stringify(report, null, 2), contentType: 'application/json' });
      await session.detach().catch(() => undefined);
    }
  });
});
