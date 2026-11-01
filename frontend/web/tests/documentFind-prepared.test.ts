import assert from 'node:assert/strict';
import { test } from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { api, ApiError, ApiTransportError } from '../src/api';
import { documentFindParameters, documentFindFragmentParameters, validateDocumentFindFragment, validateDocumentFindPage,
  type DocFindMatch, type DocFindPage, type DocumentFindPosition, type DocumentFindSource,
  type LegacyDocumentFindSource, type RootDocumentFindSource } from '../src/documentFind';
import { DocumentProjectionError } from '../src/documentVisible';
import { documentReadRetry, retryCapacityRead, retryDocumentRead, retryPendingStateRead } from '../src/readRetry';
import { DocumentFindReadError, documentFindSourceIdentity, readDocumentFindBatch } from '../src/useDocumentFind';

const documentBytes = 512 * 1024 * 1024;
const displayBytes = 512 * 1024;
const preparedPages = Math.ceil(documentBytes / 128);
const generation = `sha256:${'a'.repeat(64)}`;
const otherGeneration = `sha256:${'b'.repeat(64)}`;

const root: RootDocumentFindSource = {
  id: 'root', selection: { repoId: 'repo', hash: 'document', base: 'base', start: -1, mode: 'chat' },
  context: { hash: 'document', base: 'base', generation: 'visible_generation', rules: 1, view: 'chat', start: 2, total: 40, inherited: 2 },
  replay: 'opaque_replay-token', from: 2, to: 30,
};
const legacy: LegacyDocumentFindSource = {
  id: 'legacy', protocol: 'legacy', selection: { ...root.selection },
  context: { hash: 'document', base: 'base', view: 'chat', start: 2, total: 40, inherited: 2 }, from: 2, to: 30,
};
const sources: DocumentFindSource[] = [
  { ...root, representation: 'display-prepared-v1' },
  { ...legacy, representation: 'display-prepared-v1' },
];

function match(source: DocumentFindSource, change: Partial<DocFindMatch> = {}): DocFindMatch {
  return { prepared_page: 0, event_index: 2, field: 0, byte_offset: 4, byte_length: 6, raw: false,
    preview: '앞 NeedLE🙂 뒤', preview_byte_offset: 0, reveal_cursor: source.protocol === 'legacy' ? '' : 'opaque_reveal-token',
    source_range: { from: 120_000, to: 120_006 }, ...change };
}

function page(source: DocumentFindSource, change: Partial<DocFindPage> = {}): DocFindPage {
  return { projection: source.protocol === 'legacy' ? 'find-legacy-display-prepared-v1' : 'find-display-prepared-v1',
    hash: 'document', generation: source.protocol === 'legacy' ? '' : 'visible_generation', from: 2, to: 30,
    event_index: 3, field: 0, byte_offset: 0, prepared: { page: 0, generation: '' },
    matches: [match(source)], next_cursor: 'opaque_next-token', ...change };
}

function frontier(index = 2, preparedPage = 0, byte = 0, descriptor = generation): NonNullable<DocumentFindPosition['frontier']> {
  return { index, field: 0, byte, prepared: { page: preparedPage, generation: descriptor } };
}

test('prepared display root and legacy requests opt in explicitly on initial and opaque continuation reads', () => {
  const query = ' "Needle" + & ?\n한🙂 ';
  for (const source of sources) {
    const before = structuredClone(source);
    const initial = source.protocol === 'legacy'
      ? [['q', query], ['legacy', '1'], ['from', '2'], ['to', '30'], ['total', '40'], ['inherited', '2'], ['base', 'base']]
      : [['q', query], ['selection', root.replay], ['from', '2'], ['to', '30']];
    assert.deepEqual([...documentFindParameters(query, source)], [...initial, ['representation', 'display-prepared-v1']]);
    const cursor = 'not_decoded-or-reencoded';
    const position = { source: 7, cursor, frontier: frontier(2, 3, displayBytes + 100) };
    const previous = structuredClone(position);
    assert.deepEqual([...documentFindParameters(query, source, position)], [
      ['q', query], ...(source.protocol === 'legacy' ? [['legacy', '1']] : []), ['cursor', cursor], ['representation', 'display-prepared-v1'],
    ]);
    assert.deepEqual(position, previous);
    assert.deepEqual(source, before);
    const noBase = { ...source, from: 0, selection: { ...source.selection, base: undefined, start: 0 },
      context: { ...source.context, base: '', start: 0, inherited: 0 } };
    assert.equal(documentFindParameters(query, noBase).has('base'), false);
  }
  assert.deepEqual([...documentFindParameters(query, { ...sources[0], protocol: 'visible-v1' })],
    [...documentFindParameters(query, sources[0])]);
});

test('prepared small-event and terminal pages preserve exact wire objects and reset progress', () => {
  for (const source of sources) {
    const current = page(source, { matches: [Object.freeze(match(source))], prepared: Object.freeze({ page: 0, generation: '' }) });
    Object.freeze(current.matches);
    Object.freeze(current);
    assert.equal(validateDocumentFindPage(current, source), current);
    const terminal = page(source, { event_index: source.to, matches: [match(source, { event_index: 3 })], next_cursor: null });
    assert.equal(validateDocumentFindPage(terminal, source, frontier(2, 4, documentBytes)), terminal);
    const emptySource = { ...source, from: source.to };
    const empty = { ...terminal, from: source.to, matches: [] };
    assert.equal(validateDocumentFindPage(empty, emptySource), empty);
    assert.equal(validateDocumentFindPage(empty, emptySource, frontier(source.to, 0, 0, '')), empty);
    for (const change of [{ prepared: { page: 1, generation } }, { prepared: { page: 0, generation } },
      { field: 1 }, { byte_offset: 1 }, { next_cursor: 'not_terminal' }]) {
      assert.throws(() => validateDocumentFindPage({ ...terminal, ...change }, source), DocumentProjectionError);
    }
  }
});

test('prepared pages and matches require every own key and reject mixed-mode or unknown fields', () => {
  for (const source of sources) {
    const current = page(source);
    for (const key of Object.keys(current)) {
      const missing: Record<string, unknown> = { ...current };
      delete missing[key];
      assert.throws(() => validateDocumentFindPage(missing, source), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage(Object.assign(Object.create({ [key]: current[key as keyof DocFindPage] }), missing), source),
        DocumentProjectionError);
    }
    const selected = match(source);
    for (const key of Object.keys(selected)) {
      const missing: Record<string, unknown> = { ...selected };
      delete missing[key];
      for (const changed of [missing, Object.assign(Object.create({ [key]: selected[key as keyof DocFindMatch] }), missing)]) {
        assert.throws(() => validateDocumentFindPage({ ...current, matches: [changed] }, source), DocumentProjectionError);
      }
    }
    for (const change of [{ representation: 'display-prepared-v1' }, { version: 3 }, { unknown: true }, { prepared_page: 0 },
      { hash: 'other' }, { generation: 'other' }, { from: 1 }, { to: 31 }, { matches: null }, { matches: Array(1) }]) {
      assert.throws(() => validateDocumentFindPage({ ...current, ...change }, source), DocumentProjectionError);
    }
    for (const change of [{ generation }, { prepared: { page: 0, generation } }, { unknown: true }]) {
      assert.throws(() => validateDocumentFindPage({ ...current, matches: [{ ...selected, ...change }] }, source), DocumentProjectionError);
    }
  }
  const modes = [root, legacy].flatMap(source => [undefined, 'display-v1', 'display-prepared-v1'].map(representation => {
    const requested = { ...source, representation } as DocumentFindSource;
    const response = page(requested);
    if (representation !== 'display-prepared-v1') {
      delete response.prepared;
      delete response.matches[0].prepared_page;
      response.projection = source.protocol === 'legacy'
        ? representation ? 'find-legacy-display-v1' : 'find-legacy-v1'
        : representation ? 'find-display-v1' : 'find-v1';
    }
    return { source: requested, response };
  }));
  for (const [requestIndex, requested] of modes.entries()) {
    for (const [responseIndex, response] of modes.entries()) {
      if (requestIndex === responseIndex) assert.equal(validateDocumentFindPage(response.response, requested.source), response.response);
      else assert.throws(() => validateDocumentFindPage(response.response, requested.source), DocumentProjectionError);
    }
    if (requested.source.representation !== 'display-prepared-v1') {
      assert.throws(() => validateDocumentFindPage({ ...requested.response, prepared: { page: 0, generation: '' } }, requested.source), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage({ ...requested.response,
        matches: [{ ...requested.response.matches[0], prepared_page: 0 }] }, requested.source), DocumentProjectionError);
      assert.throws(() => documentFindParameters('needle', requested.source, { source: 0, frontier: frontier() }), DocumentProjectionError);
    }
  }
});

test('prepared progress enforces descriptor hashes, bounded page ordinals and exact nested keys', () => {
  for (const source of sources) {
    const current = page(source, { event_index: 2, matches: [], prepared: { page: 1, generation } });
    const invalidProgress = [undefined, null, [], {}, { page: 0 }, { generation: '' },
      { page: 0, generation: '', unknown: true }, { page: 1, generation: '' },
      ...[-1, 0.5, NaN, Infinity, preparedPages, Number.MAX_SAFE_INTEGER + 1, '1', null, undefined]
        .map(page => ({ page, generation })),
      ...[undefined, null, 5, false, 'generation', `sha256:${'a'.repeat(63)}`, `sha256:${'a'.repeat(65)}`,
        `sha256:${'A'.repeat(64)}`, `${generation}\n`, ` ${generation}`, `SHA256:${'a'.repeat(64)}`]
        .map(generation => ({ page: 0, generation })),
      Object.assign(Object.create({ page: 1 }), { generation }), Object.assign(Object.create({ generation }), { page: 1 }),
    ];
    for (const prepared of invalidProgress) {
      assert.throws(() => validateDocumentFindPage({ ...current, prepared }, source), DocumentProjectionError);
      const previous = { index: 2, field: 0, byte: 0, prepared } as DocumentFindPosition['frontier'];
      assert.throws(() => documentFindParameters('needle', source, { source: 0, cursor: 'opaque', frontier: previous }), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage(page(source), source, previous), DocumentProjectionError);
    }
    const maximum = { ...current, prepared: { page: preparedPages - 1, generation } };
    assert.equal(validateDocumentFindPage(maximum, source), maximum);
    const missing = { index: 2, field: 0, byte: 0 };
    assert.throws(() => documentFindParameters('needle', source, { source: 0, frontier: missing }), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(current, source, missing), DocumentProjectionError);
    const unknown = { ...frontier(), unknown: true };
    assert.throws(() => documentFindParameters('needle', source, { source: 0, frontier: unknown }), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(current, source, unknown), DocumentProjectionError);
    for (const prepared_page of [-1, 0.5, NaN, Infinity, preparedPages, '0', undefined, null]) {
      assert.throws(() => validateDocumentFindPage({ ...page(source), matches: [{ ...match(source), prepared_page }] }, source), DocumentProjectionError);
    }
  }
});

test('prepared page zero accepts large global byte coordinates only after binding a descriptor generation', () => {
  for (const source of sources) {
    const large = page(source, { event_index: 2, byte_offset: displayBytes + 20, prepared: { page: 0, generation },
      matches: [match(source, { byte_offset: displayBytes + 4, preview_byte_offset: displayBytes })] });
    assert.equal(validateDocumentFindPage(large, source), large);
    assert.equal(validateDocumentFindPage(large, source, frontier(2, 0, displayBytes + 1)), large);
    assert.equal(documentFindParameters('needle', source, { source: 0, cursor: 'opaque', frontier: frontier(2, 0, documentBytes) }).get('cursor'), 'opaque');
    const bounded = page(source, { event_index: 2, byte_offset: displayBytes, matches: [] });
    assert.equal(validateDocumentFindPage(bounded, source), bounded);
    for (const change of [{ prepared: { page: 0, generation: '' } }, { byte_offset: documentBytes + 1 }, { field: 1 }]) {
      assert.throws(() => validateDocumentFindPage({ ...large, ...change }, source), DocumentProjectionError);
    }
    assert.throws(() => documentFindParameters('needle', source, { source: 0,
      frontier: frontier(2, 0, displayBytes + 1, '') }), DocumentProjectionError);
    const nearEnd = match(source, { byte_offset: documentBytes - 4, byte_length: 4, preview: '🙂', preview_byte_offset: documentBytes - 4 });
    const maximum = page(source, { event_index: 2, byte_offset: documentBytes, prepared: { page: 0, generation }, matches: [nearEnd] });
    assert.equal(validateDocumentFindPage(maximum, source), maximum);
    for (const change of [
      { byte_offset: documentBytes, byte_length: 1, preview: 'x', preview_byte_offset: documentBytes },
      { byte_offset: documentBytes - 1, byte_length: 2, preview: 'xx', preview_byte_offset: documentBytes - 1 },
      { byte_offset: documentBytes - 1, byte_length: 1, preview: 'xx', preview_byte_offset: documentBytes - 1 },
      { byte_offset: documentBytes - 3, byte_length: 3 }, { byte_length: 0 }, { byte_length: 1025 },
    ]) {
      assert.throws(() => validateDocumentFindPage({ ...maximum, matches: [{ ...nearEnd, ...change }] }, source), DocumentProjectionError);
    }
  }
});

test('prepared frontiers advance by event then page then byte, retaining a stable generation within an event', () => {
  for (const source of sources) {
    const previous = frontier(2, 0, displayBytes + 100);
    const nextPage = page(source, { event_index: 2, byte_offset: 0, prepared: { page: 1, generation }, matches: [] });
    assert.equal(validateDocumentFindPage(nextPage, source, previous), nextPage);
    const nextEvent = page(source, { prepared: { page: 0, generation: otherGeneration }, matches: [] });
    assert.equal(validateDocumentFindPage(nextEvent, source, frontier(2, 3, documentBytes)), nextEvent);
    const smallEvent = page(source, { matches: [] });
    assert.equal(validateDocumentFindPage(smallEvent, source, frontier(2, 3, documentBytes)), smallEvent);
    for (const prepared of [{ page: 1, generation: otherGeneration }, { page: 0, generation: '' }, { page: 0, generation: otherGeneration }]) {
      assert.throws(() => validateDocumentFindPage({ ...nextPage, prepared, byte_offset: displayBytes }, source, frontier(2, 0, 10)),
        DocumentProjectionError);
    }
    for (const change of [{ prepared: { page: 1, generation }, byte_offset: 10 },
      { prepared: { page: 1, generation }, byte_offset: 9 }, { prepared: { page: 0, generation }, byte_offset: documentBytes },
      { event_index: 1 }, { field: 1 }, { next_cursor: null }]) {
      assert.throws(() => validateDocumentFindPage({ ...nextPage, ...change }, source, frontier(2, 1, 10)), DocumentProjectionError);
    }
    assert.throws(() => validateDocumentFindPage({ ...nextPage, prepared: { page: 0, generation } }, source), DocumentProjectionError);
  }
});

test('prepared match ordering includes page numbers so cross-page overlap can move global byte offsets backwards', () => {
  for (const source of sources) {
    const previous = frontier(2, 0, 600_000);
    const first = match(source, { byte_offset: 600_004, preview_byte_offset: 600_000 });
    const overlap = match(source, { prepared_page: 1, byte_offset: 599_800, preview_byte_offset: 599_796 });
    const current = page(source, { event_index: 2, byte_offset: 599_801, prepared: { page: 1, generation }, matches: [first, overlap] });
    assert.equal(validateDocumentFindPage(current, source, previous), current);
    const transition = { ...current, byte_offset: 0, matches: [first] };
    assert.equal(validateDocumentFindPage(transition, source, previous), transition);
    for (const matches of [[overlap, first], [first, first], [overlap, overlap], [{ ...first, byte_offset: 599_999, preview_byte_offset: 599_995 }],
      [{ ...first, prepared_page: 2 }], [{ ...overlap, byte_offset: 599_801, preview_byte_offset: 599_797 }]]) {
      assert.throws(() => validateDocumentFindPage({ ...current, matches }, source, previous), DocumentProjectionError);
    }
    assert.throws(() => validateDocumentFindPage({ ...current, matches: [first] }, source, frontier(2, 1, 0)), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage({ ...current, matches: [overlap] }, source, frontier(2, 1, 599_801)), DocumentProjectionError);
    const full = page(source, { event_index: 2, byte_offset: 20, prepared: { page: 1, generation },
      matches: Array.from({ length: 20 }, (_, index) => match(source, { prepared_page: 1, byte_offset: index, byte_length: 1,
        preview: 'x', preview_byte_offset: index })) });
    assert.equal(validateDocumentFindPage(full, source, frontier(2, 1)), full);
    assert.throws(() => validateDocumentFindPage({ ...full, matches: [...full.matches, match(source)] }, source), DocumentProjectionError);
  }
});

test('prepared display keeps raw false, field zero, source ranges, Unicode boundaries and reveal contracts mandatory', () => {
  for (const source of sources) {
    const current = page(source);
    for (const change of [{ raw: true }, { raw: null }, { raw: undefined }, { field: 1 }, { source_range: null },
      { source_range: { from: 0, to: documentBytes + 1 } }, { source_range: { from: 3, to: 3 } },
      { source_range: { from: 0, to: 1, extra: true } }, { preview: '\ud800' }, { byte_offset: 1 },
      { reveal_cursor: source.protocol === 'legacy' ? 'opaque' : '' }]) {
      assert.throws(() => validateDocumentFindPage({ ...current, matches: [{ ...match(source), ...change }] }, source), DocumentProjectionError);
    }
    const selected = match(source);
    const parameters = documentFindFragmentParameters(source, selected, 120_000, 64 * 1024);
    assert.deepEqual([...parameters], source.protocol === 'legacy'
      ? [['legacy', '1'], ['index', '2'], ['total', '40'], ['offset', '120000'], ['budget', '65536']]
      : [['selection', 'opaque_reveal-token'], ['offset', '120000'], ['budget', '65536']]);
    const fragment = { projection: source.protocol === 'legacy' ? 'find-legacy-fragment-v1' : 'find-fragment-v1',
      hash: 'document', generation: source.protocol === 'legacy' ? '' : 'visible_generation', event_index: 2,
      byte_offset: 120_000, total_bytes: 130_000, json_fragment: 'NeedLE🙂', next: 120_010, previous: 54_464 };
    assert.equal(validateDocumentFindFragment(fragment, source, selected, 120_000, 64 * 1024), fragment);
    const maximum = source.protocol === 'legacy' ? 2731 : 5462;
    assert.equal(documentFindParameters('needle', source, { source: 0, cursor: 'z'.repeat(maximum) }).get('cursor')?.length, maximum);
    for (const cursor of ['', 'z'.repeat(maximum + 1), 'bad=', 'bad+', 'bad/', 'bad token', 'bad\n']) {
      assert.throws(() => documentFindParameters('needle', source, { source: 0, cursor }), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage({ ...current, next_cursor: cursor }, source), DocumentProjectionError);
    }
  }
});

test('prepared batch continuations copy progress through empty scans and overlapping match pages', async context => {
  for (const source of sources) {
    const client = new QueryClient();
    const responses = [
      page(source, { event_index: 2, byte_offset: 600_000, prepared: { page: 0, generation }, matches: [], next_cursor: 'first' }),
      page(source, { event_index: 2, byte_offset: 0, prepared: { page: 1, generation },
        matches: [match(source, { byte_offset: 600_004, preview_byte_offset: 600_000 })], next_cursor: 'second' }),
      page(source, { event_index: 2, byte_offset: 600_100, prepared: { page: 1, generation },
        matches: [match(source, { prepared_page: 1, byte_offset: 599_800, preview_byte_offset: 599_796 })], next_cursor: 'third' }),
      page(source, { event_index: source.to, matches: [], next_cursor: null }),
    ];
    const calls: URLSearchParams[] = [];
    context.mock.method(api, 'getDocFindPage', async (_repo: string, _hash: string, parameters: URLSearchParams) => {
      calls.push(parameters);
      return responses[calls.length - 1];
    });
    try {
      const run = { id: source.id, query: 'needle', sources: [source] };
      const signal = new AbortController().signal;
      const first = await readDocumentFindBatch(client, run, { source: 0 }, signal);
      assert.equal(first.matches[0].match, responses[1].matches[0]);
      assert.deepEqual(first.next, { source: 0, cursor: 'second', frontier: frontier(2, 1) });
      assert.notEqual(first.next!.frontier!.prepared, responses[1].prepared);
      const second = await readDocumentFindBatch(client, run, first.next!, signal);
      assert.equal(second.matches[0].match.prepared_page, 1);
      assert.deepEqual(second.next!.frontier, frontier(2, 1, 600_100));
      const terminal = await readDocumentFindBatch(client, run, second.next!, signal);
      assert.deepEqual(terminal, { matches: [], next: undefined });
      assert.deepEqual(calls.map(parameters => parameters.get('cursor')), [null, 'first', 'second', 'third']);
      assert.ok(calls.every(parameters => parameters.get('representation') === 'display-prepared-v1'));
      assert.ok(calls.every(parameters => parameters.get('legacy') === (source.protocol === 'legacy' ? '1' : null)));
    } finally { client.clear(); context.mock.restoreAll(); }
  }
});

test('prepared mismatched context or descriptor generations fail once without replay or source fallback', async context => {
  for (const source of sources) {
    for (const change of [{ prepared: { page: 1, generation: otherGeneration } }, { prepared: { page: 0, generation: '' } },
      { hash: 'other' }, { generation: 'changed_context' }]) {
      const client = new QueryClient();
      let calls = 0;
      context.mock.method(api, 'getDocFindPage', async () => {
        calls++;
        return { ...page(source, { event_index: 2, byte_offset: 10, prepared: { page: 1, generation }, matches: [] }), ...change };
      });
      try {
        await assert.rejects(readDocumentFindBatch(client, { id: 'mismatch', query: 'needle', sources: [source, root] },
          { source: 0, cursor: 'opaque_bound_cursor', frontier: frontier(2, 0, 1) }, new AbortController().signal),
        error => error instanceof DocumentFindReadError && error.sourceId === source.id && error.cause instanceof DocumentProjectionError);
        assert.equal(calls, 1);
      } finally { client.clear(); context.mock.restoreAll(); }
    }
    assert.notEqual(documentFindSourceIdentity(source), documentFindSourceIdentity({ ...source, representation: 'display-v1' }));
    assert.notEqual(documentFindSourceIdentity(source), documentFindSourceIdentity({ ...source, representation: undefined }));
  }
});

test('source and preview batch continuations keep their original literal shapes with no prepared keys', async context => {
  for (const original of [root, legacy]) {
    for (const representation of [undefined, 'display-v1'] as const) {
      const source = { ...original, representation };
      const response = page(source);
      delete response.prepared;
      delete response.matches[0].prepared_page;
      response.projection = source.protocol === 'legacy'
        ? representation ? 'find-legacy-display-v1' : 'find-legacy-v1'
        : representation ? 'find-display-v1' : 'find-v1';
      const before = JSON.stringify(response);
      context.mock.method(api, 'getDocFindPage', async () => response);
      const client = new QueryClient();
      try {
        const result = await readDocumentFindBatch(client, { id: 'unchanged', query: 'needle', sources: [source] },
          { source: 0 }, new AbortController().signal);
        assert.deepEqual(result.next, { source: 0, cursor: 'opaque_next-token', frontier: { index: 3, field: 0, byte: 0 } });
        assert.equal(Object.hasOwn(result.next!.frontier!, 'prepared'), false);
        assert.equal(result.matches[0].match, response.matches[0]);
        assert.equal(JSON.stringify(response), before);
      } finally { client.clear(); context.mock.restoreAll(); }
    }
  }
});

test('display preparation alone receives bounded document retries, never unavailable or unsupported profiles', () => {
  const pending = new ApiError('preparing', 'doc_display_pending', 503, 1000);
  for (let attempt = 0; attempt < 12; attempt++) {
    assert.equal(documentReadRetry.retry(attempt, pending), attempt < 9);
    assert.equal(retryCapacityRead(attempt, pending), false);
    assert.equal(retryPendingStateRead(attempt, pending), false);
    const delay = documentReadRetry.retryDelay(attempt, pending);
    assert.ok(delay >= 1000 && delay <= 10_000);
  }
  assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_display_pending', 503)), true);
  for (const status of [200, 202, 400, 401, 403, 404, 409, 422, 429, 500, 502, 504]) {
    assert.equal(retryDocumentRead(0, new ApiError('rejected', 'doc_display_pending', status, 1000)), false);
  }
  for (const hint of [10_001, NaN, Infinity]) {
    assert.equal(retryDocumentRead(0, new ApiError('preparing', 'doc_display_pending', 503, hint)), false);
  }
  for (const code of ['doc_display_unavailable', 'doc_find_display_unavailable', 'doc_visible_projection_unsupported',
    'doc_find_representation_unsupported', 'integrity', 'not_found', 'unavailable']) {
    assert.equal(retryDocumentRead(0, new ApiError('terminal', code, 503, 1000)), false);
  }
  for (const error of [new ApiTransportError('offline'), new DocumentProjectionError(), new DOMException('cancelled', 'AbortError'),
    { status: 503, code: 'doc_display_pending', retryAfterMs: 1000 }]) {
    assert.equal(retryDocumentRead(0, error), false);
  }
});

test('prepared find transport preserves Retry-After one second without independent replay', async context => {
  let calls = 0;
  context.mock.method(globalThis, 'fetch', async () => {
    calls++;
    return new Response(JSON.stringify({ error: { code: 'doc_display_pending', message: 'Preparing display' } }), {
      status: 503, headers: { 'Retry-After': '1', 'Content-Type': 'application/json' },
    });
  });
  await assert.rejects(api.getDocFindPage('repo', 'document', documentFindParameters('needle', sources[0])), error => {
    assert.ok(error instanceof ApiError);
    assert.equal(error.code, 'doc_display_pending');
    assert.equal(error.status, 503);
    assert.equal(error.retryAfterMs, 1000);
    assert.equal(retryDocumentRead(0, error), true);
    return true;
  });
  assert.equal(calls, 1);
});
