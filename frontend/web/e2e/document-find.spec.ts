import { test, expect, type Page } from '@playwright/test';
import { heavyVisiblePage, visibleMessage } from '../tests/documentVisibleFixture';
import type { DocFindMatch, DocFindPage } from '../src/documentFind';
import type { DocVisiblePage } from '../src/documentVisible';

async function harness(page: Page, query = '') {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => localStorage.setItem('cxt.locale', 'en'));
  await page.route('**/__find-harness*', route => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body><div id="root"></div><script type="module">
import RefreshRuntime from '/@react-refresh';
RefreshRuntime.injectIntoGlobalHook(window);
window.$RefreshReg$ = () => {};
window.$RefreshSig$ = () => type => type;
window.__vite_plugin_react_preamble_installed__ = true;
await import('/e2e/document-paging-harness.ts');
</script></body></html>` }));
  await page.goto(`/__find-harness${query}`);
  return errors;
}

function firstPage(hash = 'document'): DocVisiblePage {
  return { ...heavyVisiblePage(), hash, replay_cursor: `self_${hash}` };
}

function result(index = 7): DocFindMatch {
  return { event_index: index, field: 0, byte_offset: 7, byte_length: 10, raw: false,
    preview: 'folded Needle🙂 detail', preview_byte_offset: 0, reveal_cursor: `reveal_${index}` };
}

function findPage(matches: DocFindMatch[], change: Partial<DocFindPage> = {}): DocFindPage {
  return { projection: 'find-v1', hash: 'document', generation: 'generation-one', from: 0, to: 620,
    event_index: 620, field: 0, byte_offset: 0, matches, next_cursor: null, ...change };
}

function revealPage(index: number): DocVisiblePage {
  return { ...firstPage(), view: 'all', start: index, from: index, to: index + 1,
    units: [{ from: index, to: index + 1, turn_index: -1, fold: null, event: { kind: 'tool_result', event_index: index,
      seq: 9000, deferred: true, event_bytes: 200, seed_summary: false, empty_reasoning: false } }],
    next_cursor: 'after_reveal', replay_cursor: `reveal_${index}` };
}

for (const legacy of [false, true]) test(`display search binds continuations and mode changes without moving the reader: legacy=${legacy}`, async ({ page }) => {
  const searches: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      searches.push(url);
      const display = url.searchParams.get('representation') === 'display-prepared-v1';
      const continuation = url.searchParams.has('cursor');
      const index = legacy ? continuation ? 2 : 1 : continuation ? 9 : 7;
      return route.fulfill({ json: findPage([{ ...result(index), ...(display ? { prepared_page: 0, source_range: { from: 500, to: 510 } } : {}),
        ...(legacy ? { reveal_cursor: '' } : {}) }], { projection: legacy ? display ? 'find-legacy-display-prepared-v1' : 'find-legacy-v1'
        : display ? 'find-display-prepared-v1' : 'find-v1', ...(display ? { prepared: { page: 0, generation: '' } } : {}), ...(legacy ? { generation: '', from: 1, to: 3 } : {}),
        event_index: continuation || !display ? legacy ? 3 : 620 : legacy ? 2 : 8,
        next_cursor: display && !continuation ? 'display_next' : null }) });
    }
    if (legacy && url.searchParams.has('projection')) return route.fulfill({ status: 409, json: { error: {
      code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } });
    if (legacy) return route.fulfill({ json: { hash: 'document', doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' },
      offset: 1, total: 3, inherited: 1, next: -1, events: [1, 2].map(index => ({ kind: 'message', event_index: index,
        role: 'user', blocks: [{ type: 'text', text: `Legacy reader ${index}` }] })) } });
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  if (legacy) await expect(page.getByText('Legacy reader 1', { exact: true })).toBeAttached();
  else await expect(page.locator('[data-visible-unit="589"]')).toBeAttached();
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = 100; });
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  const position = await scroll.evaluate(element => element.scrollTop);
  await expect(page.getByRole('combobox', { name: 'Search representation' })).toHaveValue('source');
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
  expect(searches[0].searchParams.has('representation')).toBe(false);
  await page.getByRole('combobox', { name: 'Search representation' }).selectOption('display');
  await expect(page.locator('.document-find-preview')).toHaveCount(0);
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
  await page.getByRole('button', { name: 'Next result' }).click();
  await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
  await expect.poll(() => searches.length).toBe(3);
  expect(searches.slice(1).every(url => url.searchParams.get('representation') === 'display-prepared-v1')).toBe(true);
  expect(searches[2].searchParams.get('cursor')).toBe('display_next');
  expect(searches[2].searchParams.has('selection')).toBe(false);
  expect(searches[2].searchParams.has('legacy')).toBe(legacy);
  expect(await scroll.evaluate(element => element.scrollTop)).toBe(position);
  expect(errors).toEqual([]);
});

for (const failure of ['unavailable', 'wrong-projection'] as const) test(`display search never falls back to source: ${failure}`, async ({ page }) => {
  const searches: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      searches.push(url);
      return failure === 'unavailable' ? route.fulfill({ status: 503, json: { error: {
        code: 'doc_find_display_unavailable', message: 'Display preparation unavailable', details: {} } } })
        : route.fulfill({ json: findPage([result()]) });
    }
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  await expect(page.locator('[data-visible-unit="589"]')).toBeAttached();
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('combobox', { name: 'Search representation' }).selectOption('display');
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.locator('.document-find-panel [role="alert"]')).toBeVisible();
  await expect(page.locator('.document-find-preview')).toHaveCount(0);
  expect(searches).toHaveLength(1);
  expect(searches[0].searchParams.get('representation')).toBe('display-prepared-v1');
  expect(errors).toEqual([]);
});

for (const scenario of ['decoded-tool', 'raw-large', 'decoded-legacy'] as const) test(`find opens the actual matched fragment instead of event byte zero: ${scenario}`, async ({ page }) => {
  const legacy = scenario === 'decoded-legacy';
  const raw = scenario === 'raw-large';
  const start = raw ? 200_000 : 9000;
  const selected = { ...result(legacy ? 1 : 7), source_range: { from: start, to: start + 10 },
    ...(legacy ? { reveal_cursor: '' } : {}), ...(raw ? { raw: true, byte_offset: start, preview_byte_offset: start - 7 } : {}) };
  const requests: URL[] = [];
  let fragments = 0;
  let revoked = false;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.endsWith('/find-fragments')) {
      fragments++;
      if (revoked) return route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Source access revoked', details: {} } } });
      const offset = Number(url.searchParams.get('offset'));
      const json_fragment = offset === start ? 'Needle🙂 matching canonical source' : 'previous canonical fragment';
      return route.fulfill({ json: { projection: legacy ? 'find-legacy-fragment-v1' : 'find-fragment-v1', hash: 'document',
        generation: legacy ? '' : 'generation-one', event_index: selected.event_index, byte_offset: offset, total_bytes: 400_000,
        json_fragment, next: offset + new TextEncoder().encode(json_fragment).length, previous: offset > 0 ? Math.max(0, offset - 65536) : -1 } });
    }
    if (url.pathname.endsWith('/find')) return route.fulfill({ json: findPage([selected], legacy
      ? { projection: 'find-legacy-v1', generation: '', from: 1, to: 3, event_index: 3 } : {}) });
    if (legacy && url.searchParams.has('projection')) return route.fulfill({ status: 409, json: { error: {
      code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } });
    if (legacy) return route.fulfill({ json: { hash: 'document', doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' },
      offset: 1, total: 3, inherited: 1, next: -1, events: [1, 2].map(index => ({ kind: 'message', event_index: index,
        role: 'user', blocks: [{ type: 'text', text: `Legacy reader ${index}` }] })) } });
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  if (legacy) await expect(page.getByText('Legacy reader 1', { exact: true })).toBeAttached();
  else await expect(page.locator('[data-visible-unit="589"]')).toBeAttached();
  const scroll = page.getByTestId('document-scroll');
  await scroll.evaluate(element => { element.scrollTop = 140; });
  const position = await scroll.evaluate(element => element.scrollTop);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.locator('.document-find-source mark')).toHaveText('Needle🙂');
  const request = requests.find(url => url.pathname.endsWith('/find-fragments'))!;
  expect(Object.fromEntries(request.searchParams)).toEqual(legacy
    ? { legacy: '1', index: '1', total: '3', offset: String(start), budget: '65536' }
    : { selection: 'reveal_7', offset: String(start), budget: '65536' });
  expect(requests.some(url => url.pathname.endsWith('/event-fragments') || url.searchParams.get('cursor')?.startsWith('reveal_'))).toBe(false);
  expect(await scroll.evaluate(element => element.scrollTop)).toBe(position);
  await page.getByRole('button', { name: 'Previous fragment', exact: true }).click();
  await expect(page.locator('.document-find-source')).toContainText('previous canonical fragment');
  await expect(page.getByRole('button', { name: 'Previous fragment', exact: true })).toBeFocused();
  await expect(page.locator('.document-find-source')).toHaveAttribute('tabindex', '0');
  expect(fragments).toBe(2);
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.locator('.document-find-source')).toHaveCount(0);
  revoked = true;
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.getByRole('alert')).toContainText('Source access revoked');
  await expect(page.locator('.document-find-source')).toHaveCount(0);
  expect(fragments).toBe(3);
  expect(errors).toEqual([]);
});

test('matched fragments reject changed generations without showing cached bytes or legacy fallback', async ({ page }) => {
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.endsWith('/find')) return route.fulfill({ json: findPage([{ ...result(), source_range: { from: 9000, to: 9010 } }]) });
    if (url.pathname.endsWith('/find-fragments')) return route.fulfill({ json: { projection: 'find-fragment-v1', hash: 'document',
      generation: 'changed-generation', event_index: 7, byte_offset: 9000, total_bytes: 20_000,
      json_fragment: 'Needle🙂 forbidden stale bytes', next: 9032, previous: 0 } });
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.getByRole('button', { name: 'Restart document' })).toBeVisible();
  await expect(page.locator('.document-find-source')).toHaveCount(0);
  expect(requests.filter(url => url.pathname.endsWith('/find-fragments'))).toHaveLength(1);
  expect(requests.some(url => url.pathname.endsWith('/event-fragments') || url.searchParams.get('view') === 'raw')).toBe(false);
  expect(errors).toEqual([]);
});

for (const mobile of [false, true]) test(`document find scopes loaded ranges and reveals folded source without moving the reader ${mobile ? 'mobile' : 'desktop'}`, async ({ page }, testInfo) => {
  if (mobile) await page.setViewportSize({ width: 390, height: 844 });
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.endsWith('/find')) return route.fulfill({ json: findPage([result()]) });
    if (url.searchParams.get('cursor') === 'reveal_7') return route.fulfill({ json: revealPage(7) });
    if (url.searchParams.get('view') === 'raw') return route.fulfill({ json: { hash: 'document', total: 1240, offset: 7,
      inherited: 0, next: 8, events: [{ kind: 'tool_result', event_index: 7, output: 'Exact archived folded Needle🙂 detail' }] } });
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  const scroll = page.getByTestId('document-scroll');
  await expect(page.locator('[data-visible-unit="589"]')).toBeAttached();
  await scroll.evaluate(element => { element.scrollTop = 280; });
  const before = await scroll.evaluate(element => element.scrollTop);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await expect(page.locator('.document-find-preview mark')).toHaveText('Needle🙂');
  const find = requests.find(url => url.pathname.endsWith('/find'))!;
  expect([...find.searchParams.keys()].sort()).toEqual(['from', 'q', 'selection', 'to']);
  expect(find.searchParams.get('selection')).toBe('self_document');
  expect(find.searchParams.get('from')).toBe('0');
  expect(find.searchParams.get('to')).toBe('620');
  expect(find.searchParams.get('q')).toBe('needle🙂');
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.locator('.document-find-reveal')).toContainText('Exact archived folded Needle🙂 detail');
  await expect(page.locator('.document-find-reveal [data-document-target]')).toHaveCount(0);
  expect(await scroll.evaluate(element => element.scrollTop)).toBe(before);
  expect(requests.filter(url => url.searchParams.get('cursor') === 'reveal_7')).toHaveLength(1);
  expect(requests.filter(url => url.searchParams.get('view') === 'raw')).toHaveLength(1);
  expect(requests.some(url => url.pathname.endsWith('/docs/document'))).toBe(false);
  const panelBounds = await page.locator('.document-find-panel').boundingBox();
  const scrollBounds = await scroll.boundingBox();
  expect(panelBounds!.y + panelBounds!.height).toBeLessThanOrEqual(scrollBounds!.y + scrollBounds!.height);
  await page.screenshot({ path: testInfo.outputPath(`document-find-${mobile ? 'mobile' : 'desktop'}.png`) });
  await page.getByRole('textbox', { name: 'Find text' }).press('Escape');
  await expect(page.locator('.document-find-panel')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Find in document', exact: true })).toBeFocused();
  expect(await scroll.evaluate(element => element.scrollTop)).toBe(before);
  expect(errors).toEqual([]);
});

test('document find retains one result batch and rereads previous positions', async ({ page }) => {
  const finds: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds.push(url);
      return route.fulfill({ json: url.searchParams.get('cursor') === 'next_find'
        ? findPage([result(27)])
        : findPage(Array.from({ length: 20 }, (_, index) => result(index)), { event_index: 20, next_cursor: 'next_find' }) });
    }
    return route.fulfill({ json: firstPage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  for (let number = 2; number <= 21; number++) {
    await page.getByRole('button', { name: 'Next result', exact: true }).click();
    await expect(page.getByText(`Match ${number}`, { exact: true })).toBeVisible();
  }
  expect(finds).toHaveLength(2);
  expect([...finds[1].searchParams.keys()].sort()).toEqual(['cursor', 'q']);
  await page.getByRole('button', { name: 'Previous result', exact: true }).click();
  await expect(page.getByText('Match 20', { exact: true })).toBeVisible();
  expect(finds).toHaveLength(3);
  await expect.poll(() => page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: {
      getQueryCache(): { getAll(): { queryKey: unknown[]; state: { data?: { matches?: unknown[] } } }[] };
    } }).documentQueryClient;
    return client.getQueryCache().getAll().filter(query => query.queryKey[0] === 'document-find' && query.state.data).map(query => query.state.data?.matches?.length);
  })).toEqual([20]);
});

for (const code of ['unauthenticated', 'forbidden', 'doc_visible_generation_stale']) test(`document find ${code} never recaptures or falls back`, async ({ page }) => {
  let finds = 0;
  let events = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds++;
      return route.fulfill({ status: code === 'unauthenticated' ? 401 : code === 'forbidden' ? 403 : 409,
        json: { error: { code, message: `Find denied: ${code}`, details: {} } } });
    }
    events++;
    return route.fulfill({ json: firstPage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByRole('alert')).toContainText(`Find denied: ${code}`);
  expect(finds).toBe(1);
  expect(events).toBe(1);
  if (code === 'doc_visible_generation_stale') {
    await page.getByRole('button', { name: 'Restart document' }).click();
    await expect.poll(() => events).toBe(2);
    expect(finds).toBe(1);
  } else {
    await page.getByRole('button', { name: 'Retry', exact: true }).click();
    await expect.poll(() => finds).toBe(2);
    expect(events).toBe(1);
  }
});

test('old visible responses without a replay selector explain unsupported find without fetching', async ({ page }) => {
  const requests: URL[] = [];
  await page.route('**/api/v1/**', route => {
    requests.push(new URL(route.request().url()));
    return route.fulfill({ json: heavyVisiblePage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('does not support document find yet');
  await expect(page.getByRole('button', { name: 'Search', exact: true })).toBeDisabled();
  expect(requests).toHaveLength(1);
});

test('document find includes an admitted continuation without creating a second toolbar', async ({ page }) => {
  const finds: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    const tail = url.pathname.includes('/docs/tail/');
    if (url.pathname.endsWith('/find')) {
      finds.push(url);
      return route.fulfill({ json: tail ? findPage([result(0)], { hash: 'tail', generation: 'tail-generation', to: 1, event_index: 1 })
        : findPage([]) });
    }
    if (tail) return route.fulfill({ json: { ...firstPage('tail'), base: 'document', generation: 'tail-generation', total: 1,
      from: 0, to: 1, next_cursor: null, units: [{ from: 0, to: 1, turn_index: 0,
        event: visibleMessage(0, 'Tail continuation', 'user'), fold: null }] } });
    return route.fulfill({ json: { ...heavyVisiblePage(0, 620), replay_cursor: 'self_document' } });
  });
  const errors = await harness(page, '?tail=1');
  const scroll = page.getByTestId('document-scroll');
  await expect.poll(async () => {
    await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
    return page.getByText('Tail continuation', { exact: true }).count();
  }).toBe(1);
  await expect(page.getByRole('button', { name: 'Find in document', exact: true })).toHaveCount(1);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  expect(finds.map(url => [url.pathname, url.searchParams.get('selection'), url.searchParams.get('to')])).toEqual([
    ['/api/v1/repos/repo/docs/document/find', 'self_document', '620'],
    ['/api/v1/repos/repo/docs/tail/find', 'self_tail', '1'],
  ]);
  expect(errors).toEqual([]);
});

test('document find continues through an empty bounded scan page', async ({ page }) => {
  const finds: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds.push(url);
      return route.fulfill({ json: finds.length === 1
        ? findPage([], { event_index: 128, next_cursor: 'scan_128' })
        : findPage([result(200)]) });
    }
    return route.fulfill({ json: firstPage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  expect(finds).toHaveLength(2);
  expect(finds[1].searchParams.get('cursor')).toBe('scan_128');
});

test('document find retries pending public-field preparation without downloading the full document', async ({ page }) => {
  let finds = 0;
  let events = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds++;
      if (finds === 1) return route.fulfill({ status: 503, headers: { 'Retry-After': '0' }, json: {
        error: { code: 'doc_find_fields_pending', message: 'Public fields preparing', details: {} } } });
      return route.fulfill({ json: findPage([result()]) });
    }
    events++;
    return route.fulfill({ json: firstPage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  expect(finds).toBe(2);
  expect(events).toBe(1);
});

test('legacy find preserves original identity and reveals only its selected event with fresh authority', async ({ page }) => {
  const requests: URL[] = [];
  let rawReads = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.pathname.endsWith('/find')) return route.fulfill({ json: findPage([
      { ...result(1), reveal_cursor: '' }, { ...result(2), reveal_cursor: '' },
    ], { projection: 'find-legacy-v1', generation: '', from: 1, to: 3, event_index: 3 }) });
    if (url.searchParams.get('view') === 'raw') {
      rawReads++;
      if (rawReads > 2) return route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Legacy access revoked', details: {} } } });
      const index = Number(url.searchParams.get('offset'));
      return route.fulfill({ json: { hash: 'document', doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' },
        offset: index, total: 3, inherited: 0, next: index + 1 === 3 ? -1 : index + 1,
        events: [{ kind: 'message', event_index: index, role: 'user', blocks: [{ type: 'text', text: `Legacy source Needle🙂 ${index}` }] }] } });
    }
    if (url.searchParams.has('projection')) return route.fulfill({ status: 409, json: { error: {
      code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } });
    return route.fulfill({ json: { hash: 'document', doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' }, offset: 1,
      total: 3, inherited: 1, next: -1, events: [1, 2].map(index => ({ kind: 'message', event_index: index,
        role: 'user', blocks: [{ type: 'text', text: `Legacy reader Needle🙂 ${index}` }] })) } });
  });
  const errors = await harness(page);
  await expect(page.getByText('Legacy reader Needle🙂 1', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  const find = requests.find(url => url.pathname.endsWith('/find'))!;
  expect(Object.fromEntries(find.searchParams)).toEqual({ q: 'needle🙂', legacy: '1', from: '1', to: '3', total: '3', inherited: '1', base: 'base' });
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.locator('.document-find-reveal')).toContainText('Legacy source Needle🙂 1');
  await page.getByRole('button', { name: 'Next result', exact: true }).click();
  await expect(page.getByText('Match 2', { exact: true })).toBeVisible();
  await expect(page.locator('.document-find-reveal')).toHaveCount(0);
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.locator('.document-find-reveal')).toContainText('Legacy source Needle🙂 2');
  await expect(page.locator('.document-find-reveal')).not.toContainText('Legacy source Needle🙂 1');
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.getByRole('alert')).toContainText('Legacy access revoked');
  await expect(page.locator('.document-find-reveal')).not.toContainText('Legacy source Needle🙂');
  expect(rawReads).toBe(3);
  expect(requests.filter(url => url.searchParams.get('view') === 'raw').every(url =>
    [...url.searchParams.keys()].sort().join(',') === 'limit,offset,view' && url.searchParams.get('limit') === '1')).toBe(true);
  expect(requests.every(url => url.pathname.endsWith('/events') || url.pathname.endsWith('/find'))).toBe(true);
  expect(errors).toEqual([]);
});

for (const legacyFirst of [false, true]) test(`find composes mixed identities without rewriting sources, legacy first ${legacyFirst}`, async ({ page }) => {
  const finds: URL[] = [];
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    const tail = url.pathname.includes('/docs/tail/');
    const hash = tail ? 'tail' : 'document';
    const legacy = tail !== legacyFirst;
    if (url.pathname.endsWith('/find')) {
      finds.push(url);
      return route.fulfill({ json: findPage(tail ? [{ ...result(0), reveal_cursor: legacy ? '' : 'reveal_0' }] : [], {
        hash, projection: legacy ? 'find-legacy-v1' : 'find-v1', generation: legacy ? '' : 'generation-one',
        from: 0, to: 1, event_index: 1,
      }) });
    }
    if (legacy) return url.searchParams.has('projection')
      ? route.fulfill({ status: 409, json: { error: { code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } })
      : route.fulfill({ json: { hash, doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' }, offset: 0,
        total: 1, inherited: 0, next: -1, events: [{ kind: 'message', event_index: 0, role: 'user',
          blocks: [{ type: 'text', text: tail ? 'Mixed continuation' : 'Mixed primary' }] }] } });
    return route.fulfill({ json: { ...firstPage(hash), base: tail ? 'document' : 'base', total: 1, from: 0, to: 1,
      next_cursor: null, units: [{ from: 0, to: 1, turn_index: 0, fold: null,
        event: visibleMessage(0, tail ? 'Mixed continuation' : 'Mixed primary', 'user') }] } });
  });
  const errors = await harness(page, '?tail=1');
  const scroll = page.getByTestId('document-scroll');
  await expect.poll(async () => {
    await scroll.evaluate(element => { element.scrollTop = element.scrollHeight; });
    return page.getByText('Mixed continuation', { exact: true }).count();
  }).toBe(1);
  await expect(page.getByRole('button', { name: 'Find in document', exact: true })).toHaveCount(1);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  expect(finds.map(url => [url.pathname, url.searchParams.get('legacy'), url.searchParams.get('selection')])).toEqual([
    ['/api/v1/repos/repo/docs/document/find', legacyFirst ? '1' : null, legacyFirst ? null : 'self_document'],
    ['/api/v1/repos/repo/docs/tail/find', legacyFirst ? null : '1', legacyFirst ? 'self_tail' : null],
  ]);
  expect(errors).toEqual([]);
});

for (const change of ['total', 'inherited', 'projection', 'doc_identity']) test(`legacy reveal rejects changed ${change} without a fallback`, async ({ page }) => {
  let rawReads = 0;
  const legacy = { hash: 'document', doc_identity: '', envelope: { cir_version: '1', source_provider: 'codex' }, offset: 0,
    total: 1, inherited: 0, next: -1, events: [{ kind: 'message', event_index: 0, role: 'user',
      blocks: [{ type: 'text', text: 'Private legacy needle' }] }] };
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) return route.fulfill({ json: findPage([{ ...result(0), reveal_cursor: '' }], {
      projection: 'find-legacy-v1', generation: '', to: 1, event_index: 1,
    }) });
    if (url.searchParams.get('view') === 'raw') {
      rawReads++;
      return route.fulfill({ json: change === 'projection' ? { ...legacy, projection: 'visible-v1' }
        : change === 'doc_identity' ? { ...legacy, doc_identity: 'cxt-manifest-sha256-v1' }
        : change === 'total' ? { ...legacy, total: 2, next: 1 } : { ...legacy, inherited: 1 } });
    }
    return url.searchParams.has('projection')
      ? route.fulfill({ status: 409, json: { error: { code: 'doc_visible_projection_unsupported', message: 'legacy protocol', details: {} } } })
      : route.fulfill({ json: legacy });
  });
  const errors = await harness(page);
  await expect(page.getByText('Private legacy needle', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Read the matching conversation' }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await expect(page.locator('.document-find-reveal')).not.toContainText('Private legacy needle');
  expect(rawReads).toBe(1);
  expect(errors).toEqual([]);
});

for (const identity of [undefined, 'cxt-manifest-sha256-v1']) test(`old-view response never claims legacy identity ${identity ?? 'unknown'}`, async ({ page }) => {
  let finds = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) finds++;
    return url.searchParams.has('projection')
      ? route.fulfill({ status: 409, json: { error: { code: 'doc_visible_projection_unsupported', message: 'unsupported visible contract', details: {} } } })
      : route.fulfill({ json: { hash: 'document', ...(identity ? { doc_identity: identity } : {}),
        envelope: { cir_version: '1', source_provider: 'codex' }, offset: 0, total: 1, inherited: 0, next: -1,
        events: [{ kind: 'message', event_index: 0, role: 'user', blocks: [{ type: 'text', text: 'Readable original' }] }] } });
  });
  const errors = await harness(page);
  await expect(page.getByText('Readable original', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('does not support document find yet');
  await expect(page.getByRole('button', { name: 'Search', exact: true })).toBeDisabled();
  expect(finds).toBe(0);
  expect(errors).toEqual([]);
});

test('closing document find cancels its pending action and discards late results', async ({ page }) => {
  let release: () => void = () => undefined;
  const held = new Promise<void>(resolve => { release = resolve; });
  let finds = 0;
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds++;
      await held;
      return route.fulfill({ json: findPage([result()]) }).catch(() => undefined);
    }
    return route.fulfill({ json: firstPage() });
  });
  const errors = await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect.poll(() => finds).toBe(1);
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  release();
  await expect(page.locator('.document-find-panel')).toHaveCount(0);
  await expect.poll(() => page.evaluate(() => {
    const client = (window as unknown as { documentQueryClient: {
      getQueryCache(): { getAll(): { queryKey: unknown[]; state: { data?: unknown } }[] };
    } }).documentQueryClient;
    return client.getQueryCache().getAll().filter(query => String(query.queryKey[0]).startsWith('document-find') && query.state.data).length;
  })).toBe(0);
  expect(finds).toBe(1);
  expect(errors).toEqual([]);
});

test('a denied refresh hides previously displayed find results without recapturing the document', async ({ page }) => {
  let finds = 0;
  let events = 0;
  await page.route('**/api/v1/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith('/find')) {
      finds++;
      return finds === 1 ? route.fulfill({ json: findPage([result()]) })
        : route.fulfill({ status: 403, json: { error: { code: 'forbidden', message: 'Access revoked', details: {} } } });
    }
    events++;
    return route.fulfill({ json: firstPage() });
  });
  await harness(page);
  await page.getByRole('button', { name: 'Find in document', exact: true }).click();
  await page.getByRole('textbox', { name: 'Find text' }).fill('needle🙂');
  await page.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(page.getByText('Match 1', { exact: true })).toBeVisible();
  await page.evaluate(async () => {
    const client = (window as unknown as { documentQueryClient: { invalidateQueries(options: { queryKey: string[] }): Promise<void> } }).documentQueryClient;
    await client.invalidateQueries({ queryKey: ['document-find'] });
  });
  await expect(page.getByRole('alert')).toContainText('Access revoked');
  await expect(page.locator('.document-find-result')).toHaveCount(0);
  expect(finds).toBe(2);
  expect(events).toBe(1);
});
