import assert from 'node:assert/strict';
import { test } from 'node:test';
import { InfiniteQueryObserver, QueryClient, QueryObserver } from '@tanstack/react-query';
import { ApiError } from '../src/api';
import { DOCUMENT_BODY_CACHE_BYTES, DOCUMENT_BODY_CACHE_PAGES, documentBodyKey, installDocumentBodyCache } from '../src/documentPageCache';
import { documentPageBodyOptions, recordDocumentPage, restoreDocumentPage, type DocumentPageRecord } from '../src/documentPageLedger';
import { DocumentProjectionError, documentVisibleKey } from '../src/documentVisible';
import { documentFoldQueryOptions, documentVisibleQueryOptions } from '../src/useDocumentVisible';
import { foldPage, heavyVisiblePage, visibleSelection } from './documentVisibleFixture';

function bodies(client: QueryClient) {
  return client.getQueryCache().getAll().filter(query => query.meta?.documentBody && query.state.data !== undefined);
}

test('self cursors keep initial bodies within the shared budget across many selections and replay without recapture', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const requests: Record<string, string>[] = [];
  const first = { ...heavyVisiblePage(), replay_cursor: 'self-first',
    envelope: { ...heavyVisiblePage().envelope, source_model: 'model-name', source_models: ['model-name'], context_tokens: 123,
      cwd: '/private/worktree/path', extra: 'original envelope text' } };
  let response = first;
  globalThis.fetch = async input => {
    requests.push(Object.fromEntries(new URL(String(input), 'https://fixture.invalid').searchParams));
    return new Response(JSON.stringify(response));
  };
  try {
    for (let index = 0; index < 30; index++) {
      await client.fetchInfiniteQuery(documentVisibleQueryOptions(client, visibleSelection, `selection-${index}`));
    }
    const records = client.getQueryCache().getAll().flatMap(query =>
      query.queryKey.length === 7 ? (query.state.data as { pages: DocumentPageRecord[] }).pages : []);
    assert.equal(records.length, 30);
    assert.equal(records.filter(record => 'initial' in record).length, 0);
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    for (const record of records) {
      assert.deepEqual(record.header, { source_provider: 'codex', source_model: 'model-name', source_models: ['model-name'], context_tokens: 123, output_tokens: undefined });
      const json = JSON.stringify(record);
      assert.equal(json.includes('/private/worktree'), false);
      assert.equal(json.includes('original envelope text'), false);
      assert.equal(json.includes('Readable paragraph.'), false);
    }
    assert.deepEqual(await client.fetchQuery(documentPageBodyOptions(records[0])), first);
    assert.deepEqual(requests.at(-1), { projection: 'visible-v1', cursor: 'self-first' });
    assert.equal(requests.filter(request => request.view).length, 30);
    for (const replay_cursor of [undefined, 'changed-self']) {
      response = { ...first, replay_cursor } as typeof first;
      await assert.rejects(restoreDocumentPage(records[0], new AbortController().signal), DocumentProjectionError);
    }
    assert.equal(requests.filter(request => request.view).length, 30);
  } finally { client.clear(); globalThis.fetch = original; }
});

test('empty and fully inherited pages replay with their original cursor and keep their initial header only', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const requests: Record<string, string>[] = [];
  try {
    for (const total of [0, 50]) {
      const page = { ...heavyVisiblePage(), total, inherited: total, start: total, from: total, to: total,
        units: [], next_cursor: null, replay_cursor: `self-end-${total}` };
      const record = recordDocumentPage(client, documentVisibleKey(visibleSelection), page,
        { kind: 'units', selection: visibleSelection, position: {} });
      assert.equal('initial' in record, false);
      assert.ok(record.header);
      globalThis.fetch = async input => {
        requests.push(Object.fromEntries(new URL(String(input), 'https://fixture.invalid').searchParams));
        return new Response(JSON.stringify(page));
      };
      assert.deepEqual(await restoreDocumentPage(record, new AbortController().signal), page);
    }
    assert.deepEqual(requests, [{ projection: 'visible-v1', cursor: 'self-end-0' }, { projection: 'visible-v1', cursor: 'self-end-50' }]);
  } finally { client.clear(); globalThis.fetch = original; }
});

test('a hundred old visible pages retain no initial body and eight replayable bodies', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const requests: string[] = [];
  globalThis.fetch = async input => {
    const cursor = new URL(String(input), 'https://fixture.invalid').searchParams.get('cursor') ?? '';
    requests.push(cursor);
    return new Response(JSON.stringify(heavyVisiblePage(Number(cursor.replace('main-', '')), 62_000)));
  };
  const observer = new InfiniteQueryObserver(client, documentVisibleQueryOptions(client, visibleSelection, ''));
  try {
    for (let index = 0; index < 100; index++) assert.equal((await observer.fetchNextPage()).isError, false);
    const records = observer.getCurrentResult().data!.pages as DocumentPageRecord[];
    assert.equal(records.length, 100);
    assert.equal(records.filter(record => 'initial' in record).length, 0);
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.equal(requests.length, 100);
    for (const record of records) {
      const json = JSON.stringify(record);
      for (const field of ['units', 'events', 'envelope', 'blocks']) assert.equal(json.includes(`"${field}":`), false);
      assert.equal(client.getQueryData(record.key) !== undefined, records.indexOf(record) >= 92);
    }
    const restored = await client.fetchQuery(documentPageBodyOptions(records[5]));
    assert.equal(restored.from, 3100);
    assert.deepEqual(requests.slice(100), ['main-3100']);
    assert.equal(observer.getCurrentResult().data!.pages, records);
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.deepEqual(await restoreDocumentPage(records[0], new AbortController().signal), heavyVisiblePage(0, 62_000));
    assert.deepEqual(requests.slice(100), ['main-3100', '']);
    assert.equal(requests.length, 102);
  } finally {
    observer.destroy(); client.clear(); globalThis.fetch = original;
  }
});

test('old initial visible bodies are bounded across selections and never fall back during restoration', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const requests: Record<string, string>[] = [];
  const page = heavyVisiblePage();
  let response: unknown = page;
  let status = 200;
  globalThis.fetch = async input => {
    requests.push(Object.fromEntries(new URL(String(input), 'https://fixture.invalid').searchParams));
    return new Response(JSON.stringify(response), { status });
  };
  try {
    const records = Array.from({ length: 30 }, (_, index) => recordDocumentPage(client, documentVisibleKey(visibleSelection, `old-${index}`), page,
      { kind: 'units', selection: visibleSelection, position: {} }));
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.equal(records.some(record => 'initial' in record), false);
    for (const record of records) {
      const encoded = JSON.stringify(record);
      for (const field of ['units', 'events', 'envelope', 'blocks']) assert.equal(encoded.includes(`"${field}":`), false);
      assert.equal(encoded.includes('Readable paragraph.'), false);
    }
    assert.deepEqual(await client.fetchQuery(documentPageBodyOptions(records[0])), page);
    for (const patch of [{ generation: 'changed' }, { replay_cursor: 'newer-server-self' }, { inherited: 1 }]) {
      response = { ...page, ...patch };
      if ('replay_cursor' in patch) assert.deepEqual(await restoreDocumentPage(records[0], new AbortController().signal), response);
      else await assert.rejects(restoreDocumentPage(records[0], new AbortController().signal), DocumentProjectionError);
    }
    response = { hash: page.hash, envelope: page.envelope, offset: 0, total: 1, next: -1, inherited: 0,
      events: [{ kind: 'message', role: 'user', blocks: [{ type: 'text', text: 'unexpected legacy fallback' }] }] };
    await assert.rejects(restoreDocumentPage(records[0], new AbortController().signal), DocumentProjectionError);
    for (const failure of [403, 409]) {
      status = failure;
      response = { error: { code: 'doc_visible_projection_unsupported', message: 'Denied or unsupported' } };
      await assert.rejects(restoreDocumentPage(records[0], new AbortController().signal), ApiError);
    }
    assert.equal(requests.length, 7);
    assert.equal(requests.every(request => request.projection === 'visible-v1' && request.base === visibleSelection.base && !request.cursor), true);
  } finally { client.clear(); globalThis.fetch = original; }
});

test('page restoration rejects changed generations, ranges, selectors and unsupported fallback without recapture', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const first = heavyVisiblePage(0, 1860);
  const second = heavyVisiblePage(620, 1860);
  const owner = documentVisibleKey(visibleSelection);
  const options = documentVisibleQueryOptions(client, visibleSelection, '');
  const firstRecord = recordDocumentPage(client, owner, first, { kind: 'units', selection: visibleSelection, position: {} });
  const position = options.getNextPageParam(firstRecord, [firstRecord], {}, [{}])!;
  const record = recordDocumentPage(client, owner, second, { kind: 'units', selection: visibleSelection, position });
  const requests: string[] = [];
  let response: unknown = second;
  let status = 200;
  globalThis.fetch = async input => {
    requests.push(String(input));
    return new Response(JSON.stringify(response), { status });
  };
  try {
    assert.deepEqual(await restoreDocumentPage(record, new AbortController().signal), second);
    for (const patch of [{ generation: 'another' }, { to: 1241 }, { from: 619 }, { next_cursor: 'different' }, { units: [] }]) {
      response = { ...second, ...patch };
      await assert.rejects(restoreDocumentPage(record, new AbortController().signal), DocumentProjectionError);
    }
    const changedLayout = structuredClone(second);
    changedLayout.units[0].to++;
    changedLayout.units[0].event!.event_index++;
    changedLayout.units[0].fold!.to++;
    changedLayout.units[0].fold!.rows++;
    changedLayout.units[1].from++;
    changedLayout.units[1].fold!.from++;
    changedLayout.units[1].fold!.rows--;
    const changedFold = structuredClone(second);
    changedFold.units[0].fold!.cursor = 'another-fold-selector';
    for (const changed of [changedLayout, changedFold]) {
      response = changed;
      await assert.rejects(restoreDocumentPage(record, new AbortController().signal), DocumentProjectionError);
    }
    for (const failure of [403, 409]) {
      status = failure;
      response = { error: { code: 'doc_visible_projection_unsupported', message: 'Denied or stale' } };
      await assert.rejects(restoreDocumentPage(record, new AbortController().signal), ApiError);
    }
    assert.equal(requests.length, 10);
    for (const request of requests) assert.deepEqual(Object.fromEntries(new URL(request, 'https://fixture.invalid').searchParams), { projection: 'visible-v1', cursor: 'main-620' });
  } finally { client.clear(); globalThis.fetch = original; }
});

test('fold ledger and terminal failure survive body eviction and only retry their failed incoming cursor', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const parent = heavyVisiblePage();
  const fold = parent.units[0].fold!;
  const owner = documentVisibleKey(visibleSelection);
  client.setQueryData(owner, { pages: [] });
  const options = documentFoldQueryOptions(client, visibleSelection, '', parent, fold);
  const first = foldPage(parent, 0, 0, 10);
  const second = foldPage(parent, 0, 10, 20);
  const third = foldPage(parent, 0, 20, 30);
  const requests: string[] = [];
  let rejected = true;
  globalThis.fetch = async input => {
    const cursor = new URL(String(input), 'https://fixture.invalid').searchParams.get('expand')!;
    requests.push(cursor);
    if (cursor === second.next_cursor && rejected) return new Response(JSON.stringify({ error: { code: 'forbidden', message: 'Denied' } }), { status: 403 });
    return new Response(JSON.stringify(cursor === fold.cursor ? first : cursor === first.next_cursor ? second : third));
  };
  const observer = new InfiniteQueryObserver(client, options);
  try {
    await observer.fetchNextPage();
    const admitted = await observer.fetchNextPage();
    const record = admitted.data!.pages[1];
    assert.equal((await observer.fetchNextPage()).isFetchNextPageError, true);
    assert.equal(observer.getCurrentQuery().gcTime, Infinity);
    observer.destroy();
    client.removeQueries({ queryKey: [...owner, 'body'] });
    assert.deepEqual(await client.fetchQuery(documentPageBodyOptions(record)), second);
    assert.deepEqual(requests, [fold.cursor, first.next_cursor, second.next_cursor, first.next_cursor]);
    const returning = new InfiniteQueryObserver(client, documentFoldQueryOptions(client, visibleSelection, '', parent, fold));
    const unsubscribe = returning.subscribe(() => {});
    try {
      await client.invalidateQueries({ queryKey: options.queryKey });
      assert.equal(requests.length, 4);
      assert.equal(returning.getCurrentResult().data!.pages.length, 2);
      assert.equal(returning.getCurrentResult().isFetchNextPageError, true);
      rejected = false;
      assert.equal((await returning.fetchNextPage()).data!.pages.length, 3);
      assert.equal(requests.at(-1), second.next_cursor);
    } finally { unsubscribe(); returning.destroy(); }
    client.removeQueries({ queryKey: owner, exact: true });
    assert.equal(client.getQueryCache().getAll().length, 0);
  } finally { observer.destroy(); client.clear(); globalThis.fetch = original; }
});

test('shared body budget evicts inactive data by count and bytes but retains active observers and errors', async () => {
  const client = new QueryClient();
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(visibleSelection);
  client.setQueryData(owner, { pages: [] });
  const options = (index: number) => ({ queryKey: documentBodyKey(owner, 'test', index),
    queryFn: async () => 'small', gcTime: Infinity, staleTime: Infinity,
    meta: { documentBody: true }, retry: false });
  const first = new QueryObserver(client, options(0));
  const unsubscribe = first.subscribe(() => {});
  try {
    await first.refetch();
    for (let index = 1; index < 25; index++) await client.fetchQuery(options(index));
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.equal(first.getCurrentResult().data, 'small');
    for (let index = 25; index < 35; index++) {
      const config = { ...options(index), queryFn: async () => 'x'.repeat(DOCUMENT_BODY_CACHE_BYTES / 3) };
      await client.fetchQuery(config);
    }
    assert.ok(bodies(client).reduce((total, query) => total + new TextEncoder().encode(JSON.stringify(query.state.data)).byteLength, 0) <= DOCUMENT_BODY_CACHE_BYTES);
    assert.equal(first.getCurrentResult().data, 'small');
    const denied = { ...options(0), queryFn: async (): Promise<string> => { throw new ApiError(403, 'Denied'); }, staleTime: 0 };
    await assert.rejects(client.fetchQuery(denied));
    const failed = client.getQueryCache().find({ queryKey: options(0).queryKey })!;
    assert.equal(failed.state.status, 'error');
    assert.equal(failed.state.data, undefined);
    unsubscribe(); first.destroy();
    for (let index = 35; index < 50; index++) await client.fetchQuery(options(index));
    assert.equal(client.getQueryCache().find({ queryKey: options(0).queryKey }), undefined);
    const restoredFailure = new QueryObserver(client, { ...options(0), enabled: query => query.state.status !== 'error' });
    const stop = restoredFailure.subscribe(() => {});
    assert.equal(restoredFailure.getCurrentResult().error, failed.state.error);
    assert.equal(restoredFailure.getCurrentResult().data, undefined);
    assert.equal(restoredFailure.getCurrentResult().isFetching, false);
    stop(); restoredFailure.destroy();
    client.removeQueries({ queryKey: owner, exact: true });
    assert.equal(client.getQueryCache().getAll().length, 0);
    const clearedFailure = new QueryObserver(client, { ...options(0), enabled: false });
    assert.equal(clearedFailure.getCurrentResult().error, null);
    clearedFailure.destroy();
  } finally { unsubscribe(); first.destroy(); client.clear(); }
});

test('repeated ephemeral fragment failures leave no retained body or failure tombstone', async () => {
  const client = new QueryClient();
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(visibleSelection);
  client.setQueryData(owner, { pages: [] });
  try {
    for (let index = 0; index < 100; index++) {
      const queryKey = documentBodyKey(owner, 'find-fragment', index);
      await assert.rejects(client.fetchQuery({ queryKey, retry: false, gcTime: 0,
        meta: { documentBody: true, documentEphemeral: true },
        queryFn: async () => { throw new ApiError('Denied', 'forbidden', 403); },
      }));
      assert.equal(client.getQueryCache().find({ queryKey, exact: true }), undefined);
      const restored = client.getQueryCache().build(client, { queryKey, gcTime: 0, meta: { documentBody: true } });
      assert.equal(restored.state.error, null);
      assert.equal(restored.state.status, 'pending');
      assert.equal(restored.state.data, undefined);
      client.getQueryCache().remove(restored);
    }
    assert.equal(bodies(client).length, 0);
    assert.equal(client.getQueryCache().getAll().length, 1);
  } finally { client.clear(); }
});

test('removing a reader cancels owned restoration rather than publishing into a discarded generation', async () => {
  const client = new QueryClient();
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(visibleSelection);
  client.setQueryData(owner, { pages: [] });
  let cancelled = false;
  const read = client.fetchQuery({ queryKey: documentBodyKey(owner, 'pending'), meta: { documentBody: true },
    queryFn: ({ signal }) => new Promise(resolve => {
      signal.addEventListener('abort', () => { cancelled = true; resolve('late result'); });
    }), gcTime: Infinity });
  client.removeQueries({ queryKey: owner, exact: true });
  await assert.rejects(read);
  assert.equal(cancelled, true);
  assert.equal(client.getQueryCache().getAll().length, 0);
  client.clear();
});

test('newly admitted data survives the render handoff when existing pages are all actively observed', async () => {
  const client = new QueryClient();
  installDocumentBodyCache(client);
  const owner = documentVisibleKey(visibleSelection);
  const observers: QueryObserver<string, Error>[] = [];
  const stops: (() => void)[] = [];
  let reads = 0;
  try {
    for (let index = 0; index < 20; index++) {
      const options = { queryKey: documentBodyKey(owner, 'active', index),
        queryFn: async () => { reads++; return `body-${index}`; }, gcTime: Infinity, staleTime: Infinity, meta: { documentBody: true } };
      await client.fetchQuery(options);
      assert.equal(client.getQueryData(options.queryKey), `body-${index}`);
      const observer = new QueryObserver(client, options);
      observers.push(observer);
      stops.push(observer.subscribe(() => {}));
    }
    assert.equal(reads, 20);
    assert.equal(bodies(client).length, 20);
    stops.forEach(stop => stop());
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
  } finally { stops.forEach(stop => stop()); observers.forEach(observer => observer.destroy()); client.clear(); }
});
