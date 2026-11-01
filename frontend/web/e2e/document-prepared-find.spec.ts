import { test, expect, type Page, type Route } from '@playwright/test';
import { visibleMessage } from '../tests/documentVisibleFixture';
import type { DocFindMatch, DocFindPage } from '../src/documentFind';
import type { DocVisiblePage } from '../src/documentVisible';
import type { DocViewMode } from '../src/types';

const representation = 'display-prepared-v1';
const descriptor = `sha256:${'a'.repeat(64)}`;
const changedDescriptor = `sha256:${'b'.repeat(64)}`;
const sourceOffset = 2 * 1024 * 1024;
const utf8 = (value: string) => new TextEncoder().encode(value).length;
type Respond = (route: Route, url: URL) => Promise<void>;

async function harness(page: Page, query = '', readyText = 'Reader generation-one chat 1') {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/__prepared-find-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  await page.goto(`/__prepared-find-harness${query}`);
  await expect(page.getByText(readyText, { exact: true })).toBeAttached();
  return errors;
}

function preparedMatch(legacy: boolean, change: Partial<DocFindMatch> = {}): DocFindMatch {
  return { prepared_page: 0, event_index: 1, field: 0, byte_offset: 4, byte_length: utf8('Needle🙂'), raw: false,
    preview: '앞 Needle🙂 뒤', preview_byte_offset: 0, reveal_cursor: legacy ? '' : 'reveal_event_one',
    source_range: { from: sourceOffset, to: sourceOffset + utf8('Needle🙂') }, ...change };
}

function preparedPage(legacy: boolean, change: Partial<DocFindPage> = {}): DocFindPage {
  return { projection: legacy ? 'find-legacy-display-prepared-v1' : 'find-display-prepared-v1', hash: 'document',
    generation: legacy ? '' : 'generation-one', from: 1, to: 3, event_index: 3, field: 0, byte_offset: 0,
    prepared: { page: 0, generation: '' }, matches: [], next_cursor: null, ...change };
}

function matchingPage(legacy: boolean): DocFindPage {
  return preparedPage(legacy, { event_index: 1, prepared: { page: 1, generation: descriptor },
    matches: [preparedMatch(legacy)], next_cursor: 'opaque_page_one' });
}

async function readerRoutes(page: Page, legacy: boolean, find: Respond, fragment?: Respond) {
  const requests: URL[] = [];
  const finds: URL[] = [];
  const fragments: URL[] = [];
  const unexpected: string[] = [];
  const reader = { generation: 'generation-one', total: 3 };
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.endsWith('/find')) {
      finds.push(url);
      return find(route, url);
    }
    if (url.pathname.endsWith('/find-fragments') && fragment) {
      fragments.push(url);
      return fragment(route, url);
    }
    if (url.pathname.endsWith('/events') && url.searchParams.get('view') !== 'raw') {
      if (legacy && url.searchParams.has('projection')) return route.fulfill({ status: 409, json: { error: {
        code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } });
      const view = url.searchParams.get('view') as DocViewMode;
      const events = Array.from({ length: reader.total - 1 }, (_, position) =>
        visibleMessage(position + 1, `Reader ${reader.generation} ${view} ${position + 1}`, 'user'));
      if (legacy) return route.fulfill({ json: { hash: 'document', doc_identity: '',
        envelope: { cir_version: '1', source_provider: 'codex' }, offset: 1, total: reader.total, inherited: 1, next: -1, events } });
      const body: DocVisiblePage = { projection: 'visible-v1', kind: 'units', hash: 'document', base: 'base',
        generation: reader.generation, rules: 1, view, envelope: { cir_version: '2', source_provider: 'codex' },
        total: reader.total, inherited: 1, start: 1, from: 1, to: reader.total,
        units: events.map(event => ({ from: event.event_index, to: event.event_index + 1,
          turn_index: event.event_index, event, fold: null })),
        events: [], fold: null, next_cursor: null, replay_cursor: `self_document_${reader.generation}_${view}` };
      return route.fulfill({ json: body });
    }
    unexpected.push(url.pathname + url.search);
    return route.fulfill({ status: 500, json: { error: { code: 'unexpected_read', message: 'Unexpected fallback read', details: {} } } });
  });
  return { requests, finds, fragments, unexpected, reader };
}

async function startDisplayedSearch(page: Page) {
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  const select = page.getByRole('combobox', { name: 'Search representation' });
  await expect(select).toHaveValue('source');
  await expect(select.locator('option')).toHaveText(['Source text', 'Displayed text']);
  await select.selectOption('display');
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
}

function expectPreparedRequests(capture: Awaited<ReturnType<typeof readerRoutes>>, legacy: boolean) {
  expect(capture.unexpected).toEqual([]);
  expect(capture.requests.filter(url => url.pathname.endsWith('/events'))).toHaveLength(legacy ? 2 : 1);
  for (const [index, url] of capture.finds.entries()) {
    expect(url.pathname).toBe('/api/v1/repos/repo/docs/document/find');
    expect(Object.fromEntries(url.searchParams)).toEqual(url.searchParams.has('cursor')
      ? { q: 'needle🙂', ...(legacy ? { legacy: '1' } : {}), cursor: url.searchParams.get('cursor'), representation }
      : { q: 'needle🙂', ...(legacy ? { legacy: '1', total: '3', inherited: '1', base: 'base' }
        : { selection: 'self_document_generation-one_chat' }), from: '1', to: '3', representation });
    if (index === 0) expect(url.searchParams.has('cursor')).toBe(false);
  }
}

function fragmentPage(legacy: boolean, offset: number, text: string) {
  return { projection: legacy ? 'find-legacy-fragment-v1' : 'find-fragment-v1', hash: 'document',
    generation: legacy ? '' : 'generation-one', event_index: 1, byte_offset: offset, total_bytes: 4 * 1024 * 1024,
    json_fragment: text, next: offset + utf8(text), previous: offset - 65536 };
}

function deferred() {
  let release: () => void = () => undefined;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release: () => release() };
}

for (const legacyFirst of [false, true]) {
  test(`prepared display composes ${legacyFirst ? 'legacy → Root' : 'Root → legacy'} without rewriting search or reveal identity`, async ({ page }) => {
    const requests: URL[] = [];
    const finds: URL[] = [];
    const fragments: URL[] = [];
    const unexpected: string[] = [];
    const tailLegacy = !legacyFirst;
    const tailCursor = 'opaque_tail_page_one';
    const tailMatch = preparedMatch(tailLegacy, { event_index: 0, prepared_page: 1, byte_offset: 600_004,
      preview_byte_offset: 600_000, reveal_cursor: tailLegacy ? '' : 'reveal_tail_event_zero' });
    await page.route('**/api/v1/**', route => {
      const url = new URL(route.request().url());
      requests.push(url);
      const tail = url.pathname.includes('/docs/tail/');
      const hash = tail ? 'tail' : 'document';
      const legacy = tail ? tailLegacy : legacyFirst;
      const generation = tail ? 'tail-generation' : 'generation-one';
      const from = tail ? 0 : 1;
      const total = tail ? 1 : 3;
      if (url.pathname.endsWith('/find')) {
        finds.push(url);
        if (!tail) return route.fulfill({ json: preparedPage(legacy) });
        if (!url.searchParams.has('cursor')) return route.fulfill({ json: preparedPage(legacy, {
          hash, generation: legacy ? '' : generation, from, to: total, event_index: 0,
          prepared: { page: 1, generation: descriptor }, next_cursor: tailCursor,
        }) });
        if (url.searchParams.get('cursor') === tailCursor) return route.fulfill({ json: preparedPage(legacy, {
          hash, generation: legacy ? '' : generation, from, to: total, event_index: total, matches: [tailMatch],
        }) });
      } else if (tail && url.pathname.endsWith('/find-fragments')) {
        fragments.push(url);
        return route.fulfill({ json: { ...fragmentPage(legacy, sourceOffset, 'Needle🙂 mixed prepared tail canonical bytes'),
          hash, generation: legacy ? '' : generation, event_index: 0 } });
      } else if (url.pathname.endsWith('/events') && url.searchParams.get('view') !== 'raw') {
        if (legacy && url.searchParams.has('projection')) return route.fulfill({ status: 409, json: { error: {
          code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } });
        const events = Array.from({ length: total - from }, (_, position) => visibleMessage(from + position,
          tail ? 'Mixed prepared continuation' : position === 0 ? 'Mixed prepared primary' : 'Mixed prepared primary end', 'user'));
        if (legacy) return route.fulfill({ json: { hash, doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' },
          offset: from, total, inherited: from, next: -1, events } });
        const body: DocVisiblePage = { projection: 'visible-v1', kind: 'units', hash, base: tail ? 'document' : 'base',
          generation, rules: 1, view: 'chat', envelope: { cir_version: '2', source_provider: 'codex' },
          total, inherited: from, start: from, from, to: total,
          units: events.map(event => ({ from: event.event_index, to: event.event_index + 1,
            turn_index: event.event_index, event, fold: null })),
          events: [], fold: null, next_cursor: null, replay_cursor: tail ? 'self_tail' : 'self_document_generation-one_chat' };
        return route.fulfill({ json: body });
      }
      unexpected.push(url.pathname + url.search);
      return route.fulfill({ status: 500, json: { error: { code: 'unexpected_read', message: 'Unexpected mixed-identity fallback', details: {} } } });
    });
    const errors = await harness(page, '?tail=1', 'Mixed prepared primary');
    const scroll = page.getByTestId('document-scroll');
    await expect.poll(async () => {
      await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
      return page.getByText('Mixed prepared continuation', { exact: true }).count();
    }).toBe(1);
    await expect(page.getByRole('button', { name: 'Find in document', exact: true })).toHaveCount(1);
    const readerRequests = requests.filter(url => url.pathname.endsWith('/events'));
    expect(readerRequests).toHaveLength(3);
    for (const [hash, legacy] of [['document', legacyFirst], ['tail', tailLegacy]] as const) {
      const reads = readerRequests.filter(url => url.pathname === `/api/v1/repos/repo/docs/${hash}/events`);
      expect(reads.map(url => url.searchParams.has('projection'))).toEqual(legacy ? [true, false] : [true]);
    }
    await startDisplayedSearch(page);
    await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
    await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
    await expect(page.locator('.document-find-result code')).toHaveText('tail');
    await expect(page.getByText('Search scope · records: 2 · events: 3.', { exact: true })).toBeVisible();
    expect(finds.map(url => [url.pathname, Object.fromEntries(url.searchParams)])).toEqual([
      ['/api/v1/repos/repo/docs/document/find', { q: 'needle🙂', ...(legacyFirst
        ? { legacy: '1', total: '3', inherited: '1', base: 'base' } : { selection: 'self_document_generation-one_chat' }),
      from: '1', to: '3', representation }],
      ['/api/v1/repos/repo/docs/tail/find', { q: 'needle🙂', ...(tailLegacy
        ? { legacy: '1', total: '1', inherited: '0', base: 'document' } : { selection: 'self_tail' }),
      from: '0', to: '1', representation }],
      ['/api/v1/repos/repo/docs/tail/find', { q: 'needle🙂', ...(tailLegacy ? { legacy: '1' } : {}), cursor: tailCursor, representation }],
    ]);
    await page.getByRole('button', { name: 'Read the matching conversation', exact: true }).click();
    await expect(page.locator('.document-find-source')).toHaveText('Needle🙂 mixed prepared tail canonical bytes');
    await expect(page.locator('.document-find-source mark')).toHaveText('Needle🙂');
    expect(fragments.map(url => [url.pathname, Object.fromEntries(url.searchParams)])).toEqual([
      ['/api/v1/repos/repo/docs/tail/find-fragments', tailLegacy
        ? { legacy: '1', index: '0', total: '1', offset: String(sourceOffset), budget: '65536' }
        : { selection: 'reveal_tail_event_zero', offset: String(sourceOffset), budget: '65536' }],
    ]);
    await expect(page.getByRole('button', { name: 'Next result', exact: true })).toBeDisabled();
    await expect(page.locator('.document-find-panel [role="alert"]')).toHaveCount(0);
    expect(requests.filter(url => url.pathname.endsWith('/events'))).toEqual(readerRequests);
    expect(requests).toHaveLength(readerRequests.length + finds.length + fragments.length);
    expect(finds.every(url => url.searchParams.get('representation') === representation)).toBe(true);
    expect(unexpected).toEqual([]);
    expect(errors).toEqual([]);
  });
}

for (const legacy of [false, true]) {
  const protocol = legacy ? 'legacy' : 'Root';

  test(`${protocol} Displayed text follows prepared pages and reveals exact late-page source bytes`, async ({ page }) => {
    const laterOffset = sourceOffset + 512;
    const pages = new Map<string, DocFindPage>([
      ...[8, 16, 24].map((position, index): [string, DocFindPage] => [index === 0 ? '' : `opaque_page_${position - 8}`,
        preparedPage(legacy, { event_index: 1, prepared: { page: position, generation: descriptor },
          next_cursor: `opaque_page_${position}` })]),
      ['opaque_page_24', preparedPage(legacy, { event_index: 1, prepared: { page: 32, generation: descriptor },
        matches: [preparedMatch(legacy, { prepared_page: 31, byte_offset: 600_004, preview_byte_offset: 600_000 })], next_cursor: 'opaque_page_32' })],
      ['opaque_page_32', preparedPage(legacy, { event_index: 1, byte_offset: 599_820, prepared: { page: 32, generation: descriptor },
        matches: [preparedMatch(legacy, { prepared_page: 32, byte_offset: 599_804, preview_byte_offset: 599_800,
          source_range: { from: laterOffset, to: laterOffset + utf8('Needle🙂') } })], next_cursor: 'opaque_page_32_more' })],
      ['opaque_page_32_more', preparedPage(legacy)],
    ]);
    const capture = await readerRoutes(page, legacy, (route, url) => {
      const body = pages.get(url.searchParams.get('cursor') ?? '');
      expect(body).toBeDefined();
      return route.fulfill({ json: body });
    }, (route, url) => {
      const offset = Number(url.searchParams.get('offset'));
      expect([sourceOffset, laterOffset]).toContain(offset);
      return route.fulfill({ json: fragmentPage(legacy, offset,
        `Needle🙂 canonical prepared page ${offset === sourceOffset ? 31 : 32}`) });
    });
    const errors = await harness(page);
    await startDisplayedSearch(page);
    await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
    await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
    expect(capture.finds.map(url => url.searchParams.get('cursor'))).toEqual([null, 'opaque_page_8', 'opaque_page_16', 'opaque_page_24']);
    await page.getByRole('button', { name: 'Read the matching conversation' }).click();
    await expect(page.locator('.document-find-source')).toHaveText('Needle🙂 canonical prepared page 31');
    await expect(page.locator('.document-find-source mark')).toHaveText('Needle🙂');
    await page.getByRole('button', { name: 'Next result', exact: true }).click();
    await expect(page.getByText('Match 2', { exact: true })).toBeVisible();
    await expect(page.locator('.document-find-source')).toHaveCount(0);
    await page.getByRole('button', { name: 'Read the matching conversation' }).click();
    await expect(page.locator('.document-find-source')).toHaveText('Needle🙂 canonical prepared page 32');
    await expect(page.locator('.document-find-source mark')).toHaveText('Needle🙂');
    await page.getByRole('button', { name: 'Previous result', exact: true }).click();
    await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Read the matching conversation' }).click();
    await expect(page.locator('.document-find-source')).toHaveText('Needle🙂 canonical prepared page 31');
    expect(capture.fragments.map(url => Object.fromEntries(url.searchParams))).toEqual([sourceOffset, laterOffset, sourceOffset].map(offset => legacy
      ? { legacy: '1', index: '1', total: '3', offset: String(offset), budget: '65536' }
      : { selection: 'reveal_event_one', offset: String(offset), budget: '65536' }));
    await page.getByRole('button', { name: 'Next result', exact: true }).click();
    await expect(page.getByText('Match 2', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Next result', exact: true }).click();
    await expect(page.locator('.document-find-panel [role="status"]')).toHaveText('No more matches in this range.');
    await expect(page.locator('.document-find-result')).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Next result', exact: true })).toBeDisabled();
    expect(capture.finds.map(url => url.searchParams.get('cursor'))).toEqual([
      null, 'opaque_page_8', 'opaque_page_16', 'opaque_page_24', 'opaque_page_32',
      null, 'opaque_page_8', 'opaque_page_16', 'opaque_page_24', 'opaque_page_32', 'opaque_page_32_more',
    ]);
    expectPreparedRequests(capture, legacy);
    expect(errors).toEqual([]);
  });

  test(`${protocol} prepared display retries only the same pending request`, async ({ page }) => {
    let attempts = 0;
    const capture = await readerRoutes(page, legacy, route => {
      attempts++;
      return attempts === 1 ? route.fulfill({ status: 503, headers: { 'Retry-After': '1' }, json: { error: {
        code: 'doc_display_pending', message: 'Preparing displayed text', details: {} } } })
        : route.fulfill({ json: preparedPage(legacy, { matches: [preparedMatch(legacy)] }) });
    });
    const errors = await harness(page);
    await startDisplayedSearch(page);
    await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
    expect(capture.finds).toHaveLength(2);
    expect(capture.finds[1].search).toBe(capture.finds[0].search);
    expectPreparedRequests(capture, legacy);
    expect(errors).toEqual([]);
  });

  for (const failure of ['unavailable', 'revoked'] as const) {
    test(`${protocol} prepared ${failure} hides old results without inline or raw fallback`, async ({ page }) => {
      let attempts = 0;
      const capture = await readerRoutes(page, legacy, route => {
        attempts++;
        if (attempts === 1) return route.fulfill({ json: matchingPage(legacy) });
        return route.fulfill({ status: failure === 'revoked' ? 403 : 503, headers: { 'Retry-After': '1' }, json: { error: {
          code: failure === 'revoked' ? 'forbidden' : 'doc_find_display_unavailable', message: `Prepared display ${failure}`, details: {} } } });
      });
      const errors = await harness(page);
      await startDisplayedSearch(page);
      await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
      await page.getByRole('button', { name: 'Next result', exact: true }).click();
      await expect(page.locator('.document-find-panel [role="alert"]')).toContainText(`Prepared display ${failure}`);
      await expect(page.locator('.document-find-result')).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Read the matching conversation' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Next result', exact: true })).toBeDisabled();
      expect(capture.finds).toHaveLength(2);
      expect(capture.finds[1].searchParams.get('cursor')).toBe('opaque_page_one');
      expectPreparedRequests(capture, legacy);
      expect(errors).toEqual([]);
    });
  }

  const corruptions: { name: string; change: (body: DocFindPage) => unknown }[] = [
    { name: 'descriptor-generation-change', change: body => ({ ...body, prepared: { page: 2, generation: changedDescriptor } }) },
    { name: 'missing-descriptor-generation', change: body => ({ ...body, prepared: { page: 2, generation: '' } }) },
    { name: 'regressed-page', change: body => ({ ...body, prepared: { page: 0, generation: descriptor } }) },
    { name: 'non-advancing-page', change: body => ({ ...body, prepared: { page: 1, generation: descriptor } }) },
    { name: 'unbounded-page', change: body => ({ ...body, prepared: { page: 4_194_304, generation: descriptor } }) },
    { name: 'unknown-progress-key', change: body => ({ ...body, prepared: { ...body.prepared, unknown: true } }) },
    { name: 'missing-match-page', change: body => {
      const match: Record<string, unknown> = { ...body.matches[0] };
      delete match.prepared_page;
      return { ...body, matches: [match] };
    } },
    { name: 'raw-match', change: body => ({ ...body, matches: [{ ...body.matches[0], raw: true }] }) },
    { name: 'missing-source-mapping', change: body => {
      const match: Record<string, unknown> = { ...body.matches[0] };
      delete match.source_range;
      return { ...body, matches: [match] };
    } },
    { name: 'changed-reader-generation', change: body => ({ ...body, generation: 'generation-two' }) },
    { name: 'inline-profile', change: body => {
      const inline: Record<string, unknown> = { ...body, projection: legacy ? 'find-legacy-display-v1' : 'find-display-v1', byte_offset: 20 };
      const match: Record<string, unknown> = { ...body.matches[0] };
      delete inline.prepared;
      delete match.prepared_page;
      return { ...inline, matches: [match] };
    } },
    { name: 'replayed-cursor', change: body => ({ ...body, next_cursor: 'opaque_page_one' }) },
    { name: 'terminal-descriptor-not-reset', change: body => ({ ...body, event_index: 3, next_cursor: null,
      prepared: { page: 0, generation: descriptor } }) },
  ];
  for (const corruption of corruptions) {
    test(`${protocol} rejects corrupt prepared continuation ${corruption.name} without cached results or fallback`, async ({ page }) => {
      let attempts = 0;
      const capture = await readerRoutes(page, legacy, route => {
        attempts++;
        const next = preparedPage(legacy, { event_index: 1, prepared: { page: 2, generation: descriptor },
          matches: [preparedMatch(legacy, { prepared_page: 1, preview: '앞 Needle🙂 corrupt response' })], next_cursor: 'opaque_page_two' });
        return route.fulfill({ json: attempts === 1 ? matchingPage(legacy) : corruption.change(next) });
      });
      const errors = await harness(page);
      await startDisplayedSearch(page);
      await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
      await page.getByRole('button', { name: 'Next result', exact: true }).click();
      await expect(page.locator('.document-find-panel [role="alert"]')).toBeVisible();
      await expect(page.getByRole('button', { name: 'Restart document', exact: true })).toBeVisible();
      await expect(page.locator('.document-find-result')).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Next result', exact: true })).toBeDisabled();
      expect(capture.finds).toHaveLength(2);
      expect(capture.finds[1].searchParams.get('cursor')).toBe('opaque_page_one');
      expectPreparedRequests(capture, legacy);
      expect(errors).toEqual([]);
    });
  }

  for (const failure of ['revoked', 'changed-generation'] as const) {
    test(`${protocol} prepared reveal rechecks ${failure} without reusing canonical bytes`, async ({ page }) => {
      let reveals = 0;
      const capture = await readerRoutes(page, legacy, route => route.fulfill({ json: matchingPage(legacy) }), route => {
        reveals++;
        if (reveals > 1 && failure === 'revoked') return route.fulfill({ status: 403, json: { error: {
          code: 'forbidden', message: 'Prepared source access revoked', details: {} } } });
        return route.fulfill({ json: { ...fragmentPage(legacy, sourceOffset, 'Needle🙂 original canonical bytes'),
          ...(reveals > 1 ? { generation: 'generation-two', json_fragment: 'Needle🙂 forbidden canonical data',
            next: sourceOffset + utf8('Needle🙂 forbidden canonical data') } : {}) } });
      });
      const errors = await harness(page);
      await startDisplayedSearch(page);
      await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
      const reveal = page.getByRole('button', { name: 'Read the matching conversation' });
      await reveal.click();
      await expect(page.locator('.document-find-source')).toHaveText('Needle🙂 original canonical bytes');
      await reveal.click();
      await expect(page.locator('.document-find-source')).toHaveCount(0);
      await reveal.click();
      await expect(page.locator('.document-find-reveal [role="alert"]')).toBeVisible();
      await expect(page.locator('.document-find-source')).toHaveCount(0);
      if (failure === 'changed-generation') await expect(page.getByRole('button', { name: 'Restart document', exact: true })).toBeVisible();
      else await expect(page.locator('.document-find-reveal [role="alert"]')).toContainText('Prepared source access revoked');
      expect(capture.fragments).toHaveLength(2);
      expect(capture.fragments[1].search).toBe(capture.fragments[0].search);
      expectPreparedRequests(capture, legacy);
      expect(errors).toEqual([]);
    });
  }

  for (const change of ['representation', 'view-mode'] as const) {
    test(`${protocol} changing ${change} cancels prepared search and discards its late page`, async ({ page }) => {
      const held = deferred();
      let delivered = false;
      const capture = await readerRoutes(page, legacy, async route => {
        await held.promise;
        await route.fulfill({ json: matchingPage(legacy) }).catch(() => undefined);
        delivered = true;
      });
      const errors = await harness(page);
      try {
        await startDisplayedSearch(page);
        await expect.poll(() => capture.finds.length).toBe(1);
        if (change === 'representation') await page.getByRole('combobox', { name: 'Search representation' }).selectOption('source');
        else {
          await page.getByRole('combobox', { name: 'View mode' }).selectOption('all');
          await expect(page.getByText('Reader generation-one all 1', { exact: true })).toBeAttached();
        }
        held.release();
        await expect.poll(() => delivered).toBe(true);
        await expect(page.locator('.document-find-result')).toHaveCount(0);
        await expect(page.getByRole('button', { name: 'Read the matching conversation' })).toHaveCount(0);
        await expect.poll(() => page.evaluate(() => {
          const client = (window as unknown as { documentQueryClient: {
            getQueryCache(): { getAll(): { queryKey: unknown[]; state: { data?: unknown } }[] };
          } }).documentQueryClient;
          return client.getQueryCache().getAll().filter(query => String(query.queryKey[0]).startsWith('document-find') && query.state.data).length;
        })).toBe(0);
        expect(capture.finds).toHaveLength(1);
        expect(capture.finds[0].searchParams.get('representation')).toBe(representation);
        expect(capture.unexpected).toEqual([]);
        expect(errors).toEqual([]);
      } finally { held.release(); }
    });
  }

  test(`${protocol} changing the selected source witness invalidates a pending prepared result`, async ({ page }) => {
    const held = deferred();
    let delivered = false;
    const capture = await readerRoutes(page, legacy, async route => {
      await held.promise;
      await route.fulfill({ json: matchingPage(legacy) }).catch(() => undefined);
      delivered = true;
    });
    const errors = await harness(page);
    try {
      await startDisplayedSearch(page);
      await expect.poll(() => capture.finds.length).toBe(1);
      capture.reader.generation = 'generation-two';
      if (legacy) capture.reader.total = 4;
      await page.evaluate(async () => {
        const client = (window as unknown as { documentQueryClient: { invalidateQueries(options: { queryKey: string[] }): Promise<void> } }).documentQueryClient;
        await client.invalidateQueries({ queryKey: ['doc-visible'] });
      });
      await expect(page.getByText('Reader generation-two chat 1', { exact: true })).toBeAttached();
      await expect(page.locator('.document-find-panel [role="status"]')).toHaveText('The searched record changed or closed. Start a new search.');
      held.release();
      await expect.poll(() => delivered).toBe(true);
      await expect(page.locator('.document-find-result')).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Next result', exact: true })).toBeDisabled();
      expect(capture.finds).toHaveLength(1);
      expect(capture.finds[0].searchParams.get('representation')).toBe(representation);
      expect(capture.unexpected).toEqual([]);
      expect(errors).toEqual([]);
    } finally { held.release(); }
  });
}
