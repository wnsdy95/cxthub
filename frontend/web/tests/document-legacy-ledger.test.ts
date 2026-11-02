import assert from 'node:assert/strict';
import { test } from 'node:test';
import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { ApiError, type DocViewEventPage } from '../src/api';
import { documentEventIsPrompt, documentEventIsRendered } from '../src/documentEventClassification';
import { DOCUMENT_BODY_CACHE_PAGES, documentBodyKey } from '../src/documentPageCache';
import { legacyDocumentBodyOptions, recordLegacyDocumentPage, restoreLegacyDocumentPage, type LegacyPageRecord } from '../src/documentLegacyLedger';
import { DocumentProjectionError, documentVisibleKey, validateLegacyDocument,
  type DocumentSelection, type LegacyDocumentCheckpoint, type LegacyDocumentPage } from '../src/documentVisible';
import type { DocViewEvent } from '../src/types';

const selection: DocumentSelection = { repoId: 'repository', hash: 'immutable-document', base: 'original-base', start: -1, mode: 'chat' };
const secret = 'private document body never retained by metadata';

function rawPage(offset = 2, total = 102): DocViewEventPage {
  return { hash: selection.hash, envelope: { source_provider: 'codex', source_model: 'model', source_models: ['model'],
    context_tokens: 100, output_tokens: 50, privateBody: { text: secret } } as DocViewEventPage['envelope'],
  offset, total, inherited: 2, next: offset + 1 === total ? -1 : offset + 1,
  events: [{ kind: 'message', role: 'user', seq: 10_000 - offset, blocks: [{ type: 'text', text: `${secret} ${offset}` }] }] };
}

function legacyPage(offset = 2, total = 102): LegacyDocumentPage {
  return validateLegacyDocument(rawPage(offset, total), selection, offset);
}

function bodies(client: QueryClient) {
  return client.getQueryCache().getAll().filter(query => query.meta?.documentBody && query.state.data !== undefined);
}

function objectReferences(value: unknown, found = new Set<object>()): Set<object> {
  if (value !== null && typeof value === 'object' && !found.has(value)) {
    found.add(value);
    for (const child of Object.values(value)) objectReferences(child, found);
  }
  return found;
}

function assertBodyFree(value: unknown, originals: Set<object>) {
  assert.equal(JSON.stringify(value).includes(secret), false);
  for (const reference of objectReferences(value)) {
    assert.equal(originals.has(reference), false);
    for (const field of ['events', 'event', 'envelope', 'blocks', 'input', 'output', 'initial', 'privateBody']) {
      assert.equal(field in reference, false);
    }
  }
}

test('one hundred legacy pages retain eight shared bodies and only frozen scalar metadata', async () => {
  const client = new QueryClient();
  const owner = documentVisibleKey(selection);
  const records: LegacyPageRecord[] = [];
  const originals = new Set<object>();
  const originalFetch = globalThis.fetch;
  const requests: URL[] = [];
  globalThis.fetch = async input => {
    const url = new URL(String(input), 'https://fixture.invalid');
    requests.push(url);
    return new Response(JSON.stringify(rawPage(Number(url.searchParams.get('offset')))));
  };
  try {
    client.setQueryData(owner, { pages: records });
    for (let position = 0; position < 100; position++) {
      const page = legacyPage(position + 2);
      const currentSelection = Object.assign({ ...selection }, { extra: page.envelope });
      const previous = position ? Object.assign({ kind: 'legacy' as const, next: page.offset, total: page.total }, { extra: page.events }) : undefined;
      for (const reference of objectReferences([page, currentSelection, previous])) originals.add(reference);
      const record = recordLegacyDocumentPage(client, owner, page, currentSelection, previous);
      records.push(record);
      currentSelection.hash = 'mutated';
      if (previous) previous.next = -1;
      assert.deepEqual(record.key, documentBodyKey(owner, 'legacy', page.offset));
      assert.equal(record.rows[0].index, page.offset);
      assert.equal(Object.isFrozen(record.request.selection), true);
      assert.equal(Object.isFrozen(record.rows), true);
      assert.equal(Object.isFrozen(record.rows[0]), true);
      assert.equal(Object.isFrozen(record.request), true);
      if (record.request.previous) assert.equal(Object.isFrozen(record.request.previous), true);
    }
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.equal(client.getQueryData(records[0].key), undefined);
    for (const [position, record] of records.entries()) {
      assertBodyFree(record, originals);
      assertBodyFree(record.request, originals);
      assertBodyFree(record.layout, originals);
      if (record.header) assertBodyFree(record.header, originals);
      assert.equal(client.getQueryData(record.key) !== undefined, position >= 92);
      for (const row of record.rows) assert.ok(Object.values(row).every(value => ['string', 'number', 'boolean'].includes(typeof value)));
    }
    assert.deepEqual(records[0].header, { source_provider: 'codex', source_model: 'model', source_models: ['model'], context_tokens: 100, output_tokens: 50 });
    assert.equal(records[1].header, undefined);
    assert.equal(records[0].request.selection.hash, selection.hash);
    assert.deepEqual(records[1].request.previous, { kind: 'legacy', next: 3, total: 102 });
    assert.equal((await client.fetchQuery(legacyDocumentBodyOptions(records[0]))).offset, 2);
    assert.equal((await client.fetchQuery(legacyDocumentBodyOptions(records[4]))).offset, 6);
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.deepEqual(requests.map(url => Object.fromEntries(url.searchParams)), [
      { view: 'chat', offset: '2', limit: '20', base: 'original-base' },
      { view: 'chat', offset: '6', limit: '20' },
    ]);
    assert.ok(requests.every(url => url.pathname.endsWith(`/docs/${selection.hash}/events`) && !url.searchParams.has('projection')));
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});

test('all initial bodies share the cache without a pinned legacy exception', () => {
  const client = new QueryClient();
  const records: LegacyPageRecord[] = [];
  try {
    for (let position = 0; position < 100; position++) {
      records.push(recordLegacyDocumentPage(client, documentVisibleKey(selection, `reader-${position}`), legacyPage(), selection));
    }
    assert.equal(bodies(client).length, DOCUMENT_BODY_CACHE_PAGES);
    assert.equal(client.getQueryData(records[0].key), undefined);
    assert.ok(records.every(record => !('initial' in record)));
  } finally { client.clear(); }
});

test('legacy rows match shared prompt, seed, summary and reasoning classification without using seq', () => {
  const client = new QueryClient();
  type ClassifiedEvent = DocViewEvent & { seed_summary?: boolean; empty_reasoning?: boolean };
  const events: ClassifiedEvent[] = [
    { kind: 'message', role: 'user', blocks: [{ type: 'text', text: secret }] },
    { kind: 'message', role: 'user', blocks: [{ type: 'text', text: ' \n[cxt seed] Branch-switch context: hidden' }] },
    { kind: 'message', role: 'user', blocks: [{ type: 'text', text: '\t[cxt] This session was resumed from a branch context seed. hidden' }] },
    { kind: 'message', role: 'user', compact_summary: true },
    { kind: 'message', role: 'user', seed_summary: true },
    { kind: 'message', role: 'user', seed_summary: false, blocks: [{ type: 'text', text: '[cxt seed] Branch-switch context: ordinary prompt' }] },
    { kind: 'message', role: 'user', compact_summary: true, seed_summary: false },
    { kind: 'message', role: 'assistant' },
    { kind: 'tool_call', tool_name: 'read', input: { text: secret } },
    { kind: 'reasoning' },
    { kind: 'reasoning', redacted_summary: ' \n ' },
    { kind: 'reasoning', redacted_summary: secret },
    { kind: 'reasoning', deferred: true },
    { kind: 'reasoning', deferred: true, empty_reasoning: true, redacted_summary: secret },
  ];
  const page = { ...legacyPage(2, events.length + 2), next: -1, events: events.map((event, position) => ({ ...event, seq: 200 - position })) };
  try {
    const record = recordLegacyDocumentPage(client, documentVisibleKey(selection), page, selection);
    assert.deepEqual(record.rows.map(row => row.prompt), [true, false, false, false, false, true, false, false, false, false, false, false, false, false]);
    assert.deepEqual(record.rows.map(row => row.rendered), [true, true, true, true, true, true, true, true, true, false, false, true, true, false]);
    events.forEach((event, position) => {
      assert.deepEqual(record.rows[position], { index: position + 2, kind: event.kind, ...(event.role ? { role: event.role } : {}),
        prompt: documentEventIsPrompt(event), rendered: documentEventIsRendered(event), message: event.kind === 'message' });
    });
    assertBodyFree(record, objectReferences(page));
  } finally { client.clear(); }
});

test('absent and nullable optional headers are accepted without retaining envelope objects', () => {
  const client = new QueryClient();
  try {
    for (const envelope of [{}, { source_provider: null, source_model: null, source_models: null, context_tokens: null, output_tokens: null }]) {
      const page = { ...legacyPage(), envelope: envelope as LegacyDocumentPage['envelope'] };
      const record = recordLegacyDocumentPage(client, documentVisibleKey(selection), page, selection);
      assert.deepEqual(record.header, { source_provider: undefined, source_model: undefined, source_models: undefined, context_tokens: undefined, output_tokens: undefined });
      assertBodyFree(record, objectReferences(page));
    }
  } finally { client.clear(); }
});

test('malformed event scalars, blocks, indices and headers fail admission before any body is cached', () => {
  const client = new QueryClient();
  const badEvents: unknown[] = [null, [], { kind: '' }, { kind: 'message', role: { text: secret } },
    { kind: 'message', seed_summary: {} }, { kind: 'message', compact_summary: 'true' }, { kind: 'reasoning', empty_reasoning: 1 },
    { kind: 'reasoning', deferred: 'false' }, { kind: 'reasoning', redacted_summary: {} },
    { kind: 'message', blocks: {} }, { kind: 'message', blocks: [null] }, { kind: 'message', blocks: [{ type: {} }] },
    { kind: 'message', blocks: [{ type: 'text', text: {} }] }, { kind: 'message', blocks: [{ type: 'text', name: {} }] },
    { kind: 'message', event_bytes: -1 }, { kind: 'message', event_bytes: {} }, { kind: 'message', event_index: 99 },
    { kind: 'tool_call', tool_name: {} }, { kind: 'message', agent_message: {} }, { kind: 'tool_call', input: [] }];
  const badHeaders: unknown[] = [{ source_provider: {} }, { source_model: {} }, { source_models: [secret, {}] },
    { source_models: {} }, { context_tokens: {} }, { context_tokens: -1 }, { output_tokens: 0.5 }];
  try {
    for (const event of badEvents) {
      const page = { ...legacyPage(), events: [event] } as LegacyDocumentPage;
      assert.throws(() => recordLegacyDocumentPage(client, documentVisibleKey(selection), page, selection));
      assert.equal(client.getQueryCache().getAll().length, 0);
    }
    for (const envelope of badHeaders) {
      const page = { ...legacyPage(), envelope } as LegacyDocumentPage;
      assert.throws(() => recordLegacyDocumentPage(client, documentVisibleKey(selection), page, selection), DocumentProjectionError);
      assert.equal(client.getQueryCache().getAll().length, 0);
    }
    for (const previous of [{ kind: 'legacy', next: -1, total: 102 }, { kind: 'legacy', next: 2, total: {} }]) {
      assert.throws(() => recordLegacyDocumentPage(client, documentVisibleKey(selection), legacyPage(), selection,
        previous as LegacyDocumentCheckpoint), DocumentProjectionError);
    }
    assert.throws(() => recordLegacyDocumentPage(client, [...documentVisibleKey(selection), { body: secret }], legacyPage(), selection), DocumentProjectionError);
  } finally { client.clear(); }
});

test('replays reject changed range, total, inheritance, row layout and classification without negotiation', async () => {
  const client = new QueryClient();
  const originalFetch = globalThis.fetch;
  const original = rawPage();
  const record = recordLegacyDocumentPage(client, documentVisibleKey(selection), legacyPage(), selection);
  const requests: URL[] = [];
  let response: unknown = original;
  globalThis.fetch = async input => {
    requests.push(new URL(String(input), 'https://fixture.invalid'));
    return new Response(JSON.stringify(response));
  };
  try {
    assert.equal((await restoreLegacyDocumentPage(record, new AbortController().signal)).offset, record.offset);
    for (const change of [{ hash: 'changed-document' }, { total: 103 }, { offset: 3 }, { next: -1 }, { inherited: 1 }, { doc_identity: '' }, { doc_identity: 'cxt-manifest-sha256-v1' },
      { events: [] }, { events: [{ ...original.events[0], role: 'assistant' }] },
      { events: [{ ...original.events[0], kind: 'reasoning', redacted_summary: secret }] },
      { events: [{ ...original.events[0], compact_summary: true }] }, { events: [{ ...original.events[0], seed_summary: true }] },
      { events: [{ ...original.events[0], deferred: true }] }, { events: [{ ...original.events[0], empty_reasoning: true }] },
      { events: [{ ...original.events[0], event_bytes: 123 }] }, { events: [{ ...original.events[0], role: {} }] },
      { envelope: { source_models: [{}] } }, { projection: 'visible-v1' },
    ]) {
      response = { ...original, ...change };
      await assert.rejects(restoreLegacyDocumentPage(record, new AbortController().signal));
    }
    response = { ...original, events: [{ ...original.events[0], seq: -100, blocks: [{ type: 'text', text: 'different ordinary prompt' }] }] };
    assert.equal((await restoreLegacyDocumentPage(record, new AbortController().signal)).events[0].seq, -100);
    assert.ok(requests.every(url => !url.searchParams.has('projection') && url.searchParams.get('offset') === '2' && url.searchParams.get('base') === selection.base));
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});

test('reasoning and seed-prefix reclassification is rejected even without explicit flags', async () => {
  const client = new QueryClient();
  const originalFetch = globalThis.fetch;
  try {
    for (const [initial, changed] of [
      [{ kind: 'message', role: 'user', blocks: [{ type: 'text', text: secret }] },
        { kind: 'message', role: 'user', blocks: [{ type: 'text', text: '[cxt seed] Branch-switch context: changed' }] }],
      [{ kind: 'reasoning', redacted_summary: secret }, { kind: 'reasoning', redacted_summary: ' \n ' }],
      [{ kind: 'message', role: 'assistant', compact_summary: true, blocks: [{ type: 'text', text: secret }] },
        { kind: 'message', role: 'assistant', compact_summary: true, blocks: [{ type: 'text', text: '[cxt seed] Branch-switch context: changed' }] }],
    ]) {
      const page = { ...legacyPage(), events: [initial] };
      const record = recordLegacyDocumentPage(client, documentVisibleKey(selection), page, selection);
      globalThis.fetch = async () => new Response(JSON.stringify({ ...rawPage(), events: [changed] }));
      await assert.rejects(restoreLegacyDocumentPage(record, new AbortController().signal), DocumentProjectionError);
    }
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});

test('terminal permission and malformed failures never fall back or silently retry on remount', async () => {
  const client = new QueryClient();
  const originalFetch = globalThis.fetch;
  const owner = documentVisibleKey(selection);
  client.setQueryData(owner, { pages: [] });
  const record = recordLegacyDocumentPage(client, owner, legacyPage(3), selection, { kind: 'legacy', next: 3, total: 102 });
  const requests: URL[] = [];
  let status = 403;
  globalThis.fetch = async input => {
    requests.push(new URL(String(input), 'https://fixture.invalid'));
    return new Response(JSON.stringify({ error: { code: 'doc_visible_projection_unsupported', message: 'Denied' } }), { status });
  };
  try {
    for (const failure of [401, 403, 404, 409]) {
      status = failure;
      await assert.rejects(restoreLegacyDocumentPage(record, new AbortController().signal), error => error instanceof ApiError && error.status === failure);
    }
    const options = { ...legacyDocumentBodyOptions(record), staleTime: 0 };
    await assert.rejects(client.fetchQuery(options), ApiError);
    assert.equal(client.getQueryData(record.key), undefined);
    const requestCount = requests.length;
    const observer = new QueryObserver(client, legacyDocumentBodyOptions(record));
    const unsubscribe = observer.subscribe(() => {});
    try {
      assert.equal(observer.getCurrentResult().isError, true);
      assert.equal(observer.getCurrentResult().isFetching, false);
      assert.equal(observer.getCurrentResult().data, undefined);
      await client.invalidateQueries({ queryKey: record.key });
      assert.equal(requests.length, requestCount);
    } finally { unsubscribe(); observer.destroy(); }
    assert.ok(requests.every(url => !url.searchParams.has('base') && !url.searchParams.has('projection') && url.searchParams.get('offset') === '3'));
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});

test('initial empty and fully inherited pages replay their explicit admitted offset', async () => {
  const client = new QueryClient();
  const originalFetch = globalThis.fetch;
  const requests: URL[] = [];
  try {
    for (const total of [0, 50]) {
      const raw = { ...rawPage(), total, offset: total, inherited: total, next: -1, events: [] };
      const record = recordLegacyDocumentPage(client, documentVisibleKey(selection), validateLegacyDocument(raw, selection, -1), selection);
      assert.deepEqual(record.rows, []);
      globalThis.fetch = async input => {
        requests.push(new URL(String(input), 'https://fixture.invalid'));
        return new Response(JSON.stringify(raw));
      };
      assert.equal((await restoreLegacyDocumentPage(record, new AbortController().signal)).offset, total);
    }
    assert.deepEqual(requests.map(url => url.searchParams.get('offset')), ['0', '50']);
    assert.ok(requests.every(url => url.searchParams.get('base') === selection.base));
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});

test('removing a reader cancels only owned legacy restoration and cannot admit its late body', async () => {
  const client = new QueryClient();
  const originalFetch = globalThis.fetch;
  const owner = documentVisibleKey(selection, 'discarded');
  const unrelatedOwner = documentVisibleKey(selection, 'retained');
  client.setQueryData(owner, { pages: [] });
  client.setQueryData(unrelatedOwner, { pages: [] });
  const record = recordLegacyDocumentPage(client, owner, legacyPage(), selection);
  const unrelated = recordLegacyDocumentPage(client, unrelatedOwner, legacyPage(), selection);
  client.removeQueries({ queryKey: record.key, exact: true });
  let cancelled = false;
  let beginRead: () => void = () => {};
  let finishRead: (response: Response) => void = () => {};
  const started = new Promise<void>(resolve => { beginRead = resolve; });
  globalThis.fetch = async (_input, init) => new Promise<Response>(resolve => {
    finishRead = resolve;
    init?.signal?.addEventListener('abort', () => { cancelled = true; });
    beginRead();
  });
  try {
    const read = client.fetchQuery(legacyDocumentBodyOptions(record));
    const rejected = assert.rejects(read);
    await started;
    client.removeQueries({ queryKey: owner, exact: true });
    await rejected;
    assert.equal(cancelled, true);
    finishRead(new Response(JSON.stringify(rawPage())));
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(client.getQueryData(record.key), undefined);
    assert.equal(client.getQueryData(unrelated.key) !== undefined, true);
    assert.equal(bodies(client).length, 1);
  } finally { client.clear(); globalThis.fetch = originalFetch; }
});
