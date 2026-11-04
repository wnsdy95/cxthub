import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ApiError } from '../src/api';
import { documentStreamCheckpoint, documentVisibleKey, DocumentProjectionError, validateLegacyDocument, validateVisibleFold, validateVisibleUnits,
  visibleContext, visibleContextWitness, visiblePageCheckpoint, visibleParameters,
  type DocVisiblePage } from '../src/documentVisible';
import { readDocumentFold, readDocumentStream } from '../src/documentVisibleReads';
import { clippedFoldPage, clippedPromptPage, foldPage, heavyVisiblePage, visibleMessage, visibleSelection } from './documentVisibleFixture';
import { containsCursor, historyCursors, rememberCursor, type CursorHistory } from '../src/documentCursorHistory';
import { InfiniteQueryObserver, QueryClient } from '@tanstack/react-query';
import { documentFoldQueryOptions, documentVisibleQueryOptions, documentVisibleReadOptions } from '../src/useDocumentVisible';
import { recordDocumentPage } from '../src/documentPageLedger';

function objectReferences(value: unknown, found = new Set<object>()): Set<object> {
  if (value !== null && typeof value === 'object' && !found.has(value)) {
    found.add(value);
    for (const child of Object.values(value)) objectReferences(child, found);
  }
  return found;
}

function assertBodyFree(witness: unknown, bodies: Set<object>) {
  for (const reference of objectReferences(witness)) {
    assert.equal(bodies.has(reference), false);
    for (const field of ['units', 'events', 'envelope', 'event', 'blocks', 'input']) assert.equal(field in reference, false);
  }
}

test('old-view checkpoints preserve actual document identity separately from their transport', () => {
  const raw = { hash: 'document', doc_identity: '', envelope: heavyVisiblePage().envelope, offset: 0,
    total: 2, inherited: 0, next: 1, events: [visibleMessage(0, 'Legacy payload')] };
  const first = validateLegacyDocument(raw, visibleSelection, 0);
  const checkpoint = documentStreamCheckpoint(first);
  if (checkpoint.kind !== 'legacy') throw new Error('Expected old-view checkpoint');
  assert.deepEqual(checkpoint, { kind: 'legacy', doc_identity: '', next: 1, total: 2 });
  const next = { ...raw, offset: 1, next: -1, events: [visibleMessage(1, 'Next legacy payload')] };
  assert.equal(validateLegacyDocument(next, visibleSelection, 1, checkpoint).doc_identity, '');
  for (const identity of [undefined, 'cxt-manifest-sha256-v1', 'invalid', null]) {
    assert.throws(() => validateLegacyDocument({ ...next, doc_identity: identity }, visibleSelection, 1, checkpoint), DocumentProjectionError);
  }
  assert.equal(validateLegacyDocument({ ...raw, doc_identity: 'cxt-manifest-sha256-v1' }, visibleSelection, 0).kind, 'legacy');
});

test('context witnesses and actual continuation page parameters copy only body-free metadata', () => {
  const client = new QueryClient();
  const first = heavyVisiblePage();
  const context = clippedPromptPage();
  const folded = clippedFoldPage(context, 10, 15);
  const legacy = validateLegacyDocument({ hash: 'document', envelope: first.envelope, offset: 0, total: 2, inherited: 0, next: 1,
    events: [visibleMessage(0, 'Legacy payload')] }, visibleSelection, 0);
  Object.assign(first, { extraPayload: first.units });
  Object.assign(context.units[0].fold!, { extraPayload: context.envelope });
  const bodies = objectReferences([first, context, folded, legacy]);
  const witness = visibleContextWitness(first);
  assert.deepEqual(witness, { hash: 'document', base: 'base', generation: 'generation-one', rules: 1, view: 'chat',
    start: 0, total: 1240, inherited: 0 });
  assert.deepEqual(visibleContext(witness), visibleContext(first));
  const options = documentVisibleQueryOptions(client, visibleSelection, 'restart');
  const firstRecord = recordDocumentPage(client, options.queryKey, first, { kind: 'units', selection: visibleSelection, position: options.initialPageParam });
  const position = options.getNextPageParam(firstRecord, [firstRecord], options.initialPageParam, [options.initialPageParam])!;
  assert.deepEqual(position.previous, { ...witness, kind: 'units', to: 620, next_cursor: 'main-620', lastTurnIndex: -1, fold: null });
  const legacyPosition = options.getNextPageParam(legacy, [legacy], options.initialPageParam, [options.initialPageParam])!;
  assert.deepEqual(legacyPosition, { previous: { kind: 'legacy', next: 1, total: 2 } });
  assert.equal(options.getNextPageParam({ ...legacy, next: -1 }, [legacy], legacyPosition, [legacyPosition]), undefined);
  const foldedSelection = { ...visibleSelection, mode: 'prompts' as const };
  const foldedOptions = documentFoldQueryOptions(client, foldedSelection, 'restart', context, context.units[0].fold!);
  const foldedRecord = recordDocumentPage(client, options.queryKey, folded, { kind: 'fold', selection: foldedSelection,
    context, fold: context.units[0].fold!, position: foldedOptions.initialPageParam });
  const foldedPosition = foldedOptions.getNextPageParam(foldedRecord, [foldedRecord], foldedOptions.initialPageParam, [foldedOptions.initialPageParam])!;
  assert.equal(foldedPosition.renderedBefore, 4);
  assert.deepEqual(foldedPosition.previous, { ...visibleContextWitness(context), kind: 'fold', to: 15, next_cursor: 'fold-next-15',
    lastTurnIndex: -1, fold: { from: 10, to: 30 } });
  assert.deepEqual([...historyCursors(position.history)], ['main-620']);
  assert.deepEqual([...historyCursors(foldedPosition.history)], ['clipped-fold']);
  for (const metadata of [witness, visiblePageCheckpoint(first), documentStreamCheckpoint(legacy), position, legacyPosition, foldedPosition]) {
    assertBodyFree(metadata, bodies);
  }
  const expected = structuredClone([witness, position, legacyPosition, foldedPosition]);
  first.generation = 'changed';
  first.units.at(-1)!.turn_index = 600;
  first.to = 1;
  first.next_cursor = 'changed';
  folded.fold!.to = 100;
  folded.events.length = 0;
  legacy.next = -1;
  legacy.total = 100;
  assert.deepEqual([witness, position, legacyPosition, foldedPosition], expected);
  client.clear();
});

test('cursor histories retain one shared immutable node per page, support branching refetches and reject loops', () => {
  const positions: CursorHistory[] = [];
  for (let index = 0; index < 10_000; index++) {
    const previous = positions.at(-1);
    const history = Object.freeze(rememberCursor(`cursor-${index}`, previous));
    assert.equal(history.previous, previous);
    assert.equal(Object.keys(history).length, 2);
    positions.push(history);
  }
  const unique = new Set<CursorHistory>();
  for (let node: CursorHistory | undefined = positions.at(-1); node; node = node.previous) unique.add(node);
  assert.equal(unique.size, positions.length);
  assert.equal([...historyCursors(positions.at(-1))].length, positions.length);
  const refetch = rememberCursor('fresh-cursor', positions[4]);
  assert.equal(refetch.previous, positions[4]);
  assert.equal(positions[5].previous, positions[4]);
  assert.equal(positions[5].cursor, 'cursor-5');
  assert.equal(containsCursor(historyCursors(refetch), 'cursor-4'), true);
  assert.equal(containsCursor(historyCursors(refetch), 'cursor-5'), false);
  assert.equal(containsCursor(historyCursors(positions.at(-1)), 'fresh-cursor'), false);
  const client = new QueryClient();
  const key = ['linear-history'];
  client.setQueryDefaults(key, documentVisibleReadOptions);
  client.setQueryData(key, { pageParams: positions.slice(0, 5000).map(history => ({ history })) });
  const pageParams = positions.map(history => ({ history }));
  client.setQueryData(key, { pageParams });
  assert.equal(client.getQueryData<{ pageParams: unknown }>(key)!.pageParams, pageParams);
  const refetched: CursorHistory[] = [];
  for (let index = 0; index < positions.length; index++) refetched.push(rememberCursor(`refetched-${index}`, refetched.at(-1)));
  const refreshedParams = refetched.map(history => ({ history }));
  client.setQueryData(key, { pageParams: refreshedParams });
  const retained = client.getQueryData<{ pageParams: { history: CursorHistory }[] }>(key)!.pageParams;
  retained.forEach((position, index) => {
    assert.equal(position.history, refetched[index]);
    assert.equal(position.history.previous, retained[index - 1]?.history);
  });
  client.clear();
  const first = heavyVisiblePage(0, 1860);
  const next = heavyVisiblePage(620, 1860);
  const loop = rememberCursor(next.next_cursor!, rememberCursor(first.next_cursor!));
  assert.throws(() => validateVisibleUnits(next, visibleSelection, first, historyCursors(loop)), DocumentProjectionError);
  assert.equal(validateVisibleUnits(next, visibleSelection, first, historyCursors(loop.previous)), next);
  assert.throws(() => validateVisibleUnits(next, visibleSelection, visiblePageCheckpoint(first), historyCursors(loop)), DocumentProjectionError);
  assert.equal(validateVisibleUnits(next, visibleSelection, visiblePageCheckpoint(first), historyCursors(loop.previous)), next);
  const fold = first.units[0].fold!;
  const chunk = foldPage(first, 0, 0, 10);
  assert.throws(() => validateVisibleFold(chunk, visibleSelection, first, fold, undefined, 0,
    historyCursors(rememberCursor(chunk.next_cursor!))), DocumentProjectionError);
});

test('visible pages cover 620 raw events with twenty ordered anchors and opaque selectors', () => {
  const first = heavyVisiblePage();
  const next = heavyVisiblePage(620);
  assert.equal(validateVisibleUnits(first, visibleSelection), first);
  assert.equal(first.units.length, 20);
  assert.equal(first.events.length, 0);
  assert.equal(validateVisibleUnits(next, visibleSelection, first, [first.next_cursor!]), next);
  assert.equal(validateVisibleUnits(next, visibleSelection, visiblePageCheckpoint(first), [first.next_cursor!]), next);
  assert.deepEqual(Object.fromEntries(visibleParameters(visibleSelection)), { projection: 'visible-v1', view: 'chat', offset: '-1', limit: '20', base: 'base' });
  assert.deepEqual(Object.fromEntries(visibleParameters(visibleSelection, { kind: 'units', cursor: 'opaque+/=' })), { projection: 'visible-v1', cursor: 'opaque+/=' });
  assert.deepEqual(Object.fromEntries(visibleParameters(visibleSelection, { kind: 'fold', cursor: 'opaque+/=' })), { projection: 'visible-v1', expand: 'opaque+/=' });
  for (const change of [{ repoId: 'other' }, { hash: 'other' }, { base: 'other' }, { start: 10 }, { mode: 'all' as const }]) {
    assert.notDeepEqual(documentVisibleKey(visibleSelection), documentVisibleKey({ ...visibleSelection, ...change }));
  }
  assert.notDeepEqual(documentVisibleKey(visibleSelection, 'fresh-capture'), documentVisibleKey(visibleSelection));
});

test('visible unit validation rejects malformed, sparse, overlapping, gapped and stale pages before append', () => {
  const first = heavyVisiblePage();
  const next = heavyVisiblePage(620);
  const bad = (value: unknown, previous?: DocVisiblePage) => {
    for (const checkpoint of [previous, previous ? visiblePageCheckpoint(previous) : undefined]) {
      assert.throws(() => validateVisibleUnits(value, visibleSelection, checkpoint, previous ? ['main-620'] : []), DocumentProjectionError);
    }
  };
  for (const invalid of [null, {}, [], { ...first, projection: null }, { ...first, kind: 'fold' }, { ...first, units: null },
    { ...first, events: null }, { ...first, events: [visibleMessage(0, 'bad')] }, { ...first, next_cursor: undefined },
    { ...first, next_cursor: '' }, { ...first, next_cursor: -1 }, { ...first, next_cursor: null }, { ...first, from: 1 },
    { ...first, to: 619 }, { ...first, units: [] }, { ...first, rules: 2 }, { ...first, generation: '' },
    { ...first, base: 'wrong' }, { ...first, view: 'all' }, { ...first, hash: 'other' }, { ...first, start: 1 },
    { ...first, inherited: -1 }, { ...first, fold: { from: 0, to: 30 } },
    { ...first, units: [...first.units, first.units[0]] },
  ]) bad(invalid);
  for (const mutate of [
    (page: DocVisiblePage) => { page.units[1].from--; },
    (page: DocVisiblePage) => { page.units[1].from++; },
    (page: DocVisiblePage) => { page.units[0].event!.event_index--; },
    (page: DocVisiblePage) => { page.units[0].fold!.to++; },
    (page: DocVisiblePage) => { page.units[0].fold!.from++; },
    (page: DocVisiblePage) => { page.units[0].fold!.rows = 31; },
    (page: DocVisiblePage) => { page.units[0].fold = null; },
    (page: DocVisiblePage) => { page.units[0].turn_index = 50; },
    (page: DocVisiblePage) => { page.units[0].event!.empty_reasoning = true; },
    (page: DocVisiblePage) => { delete (page.units[0].event as Partial<typeof page.units[0]['event']>)!.seed_summary; },
    (page: DocVisiblePage) => { delete (page.units[0].event as Partial<typeof page.units[0]['event']>)!.empty_reasoning; },
  ]) {
    const invalid = structuredClone(first);
    mutate(invalid);
    bad(invalid);
  }
  for (const change of [{ hash: 'wrong' }, { base: 'wrong' }, { generation: 'new' }, { rules: 2 }, { view: 'prompts' }, { start: 1 }, { total: 1300 }, { inherited: 10 }]) bad({ ...next, ...change }, first);
  bad(first, first);
  bad({ ...next, from: 621 }, first);
  bad({ ...next, from: 619 }, first);
  bad({ ...next, total: 1300, next_cursor: 'main-620' }, { ...first, total: 1300 });
  bad({ ...next, units: [], from: next.to }, first);
  bad(next, { ...first, kind: 'fold' });
  bad(next, { ...first, next_cursor: null });
});

test('unit checkpoints preserve the previous turn ordinal rather than resetting the turn at a page boundary', () => {
  const first = heavyVisiblePage();
  first.units.at(-1)!.turn_index = 588;
  validateVisibleUnits(first, visibleSelection);
  const checkpoint = visiblePageCheckpoint(first);
  assert.equal(checkpoint.lastTurnIndex, 588);
  const next = heavyVisiblePage(620);
  next.units.forEach(unit => { unit.turn_index = 588; });
  for (const previous of [first, checkpoint]) assert.equal(validateVisibleUnits(next, visibleSelection, previous), next);
  next.units[0].turn_index = 587;
  for (const previous of [first, checkpoint]) {
    assert.throws(() => validateVisibleUnits(next, visibleSelection, previous), DocumentProjectionError);
  }
});

test('visible units and folds reject nonboolean summary flags before metadata admission', () => {
  const first = heavyVisiblePage();
  const folded = foldPage(first, 0, 0, 30);
  for (const compact_summary of [{ payload: 'must not enter metadata'.repeat(400) }, [], 'true', 1, null]) {
    const units = structuredClone(first);
    Object.assign(units.units[0].event!, { compact_summary });
    assert.throws(() => validateVisibleUnits(units, visibleSelection), DocumentProjectionError);
    const detail = structuredClone(folded);
    Object.assign(detail.events[0], { compact_summary });
    assert.throws(() => validateVisibleFold(detail, visibleSelection, first, first.units[0].fold!), DocumentProjectionError);
  }
  for (const compact_summary of [true, false, undefined]) {
    const units = structuredClone(first);
    units.units[0].event!.compact_summary = compact_summary;
    assert.equal(validateVisibleUnits(units, visibleSelection), units);
  }
});

test('reader headers accept only scalar model/token fields and string model lists', () => {
  const first = heavyVisiblePage();
  const legacy = { hash: 'document', envelope: first.envelope, offset: 0, total: 1, inherited: 0, next: -1,
    events: [visibleMessage(0, 'Legacy header')] };
  for (const invalid of [
    { source_provider: { body: 'not scalar' } }, { source_model: ['not scalar'] },
    { source_models: { slice: 'not an array' } }, { source_models: [{ body: 'not a model name' }] },
    { context_tokens: { body: 'not a number' } }, { output_tokens: '100' }, { context_tokens: -1 }, { output_tokens: 1.5 },
  ]) {
    assert.throws(() => validateVisibleUnits({ ...first, envelope: invalid }, visibleSelection), DocumentProjectionError);
    assert.throws(() => validateLegacyDocument({ ...legacy, envelope: invalid }, visibleSelection, 0), DocumentProjectionError);
  }
  for (const envelope of [{}, { source_model: 'model', source_models: ['model'], context_tokens: 0, output_tokens: 123 },
    { source_provider: null, source_model: null, source_models: null, context_tokens: null, output_tokens: null }]) {
    assert.ok(validateVisibleUnits({ ...first, envelope }, visibleSelection));
    assert.ok(validateLegacyDocument({ ...legacy, envelope }, visibleSelection, 0));
  }
});

test('clipped inherited prompt units keep seed summaries out of anchors and empty reasoning out of row counts', () => {
  const selection = { ...visibleSelection, mode: 'prompts' as const };
  const page = clippedPromptPage();
  assert.equal(validateVisibleUnits(page, selection), page);
  assert.equal(page.units[0].event, null);
  assert.equal(page.units[0].turn_index, 0);
  const fold = page.units[0].fold!;
  const first = clippedFoldPage(page, 10, 15);
  const next = clippedFoldPage(page, 15, 30);
  assert.equal(validateVisibleFold(first, selection, page, fold), first);
  assert.equal(validateVisibleFold(next, selection, page, fold, first, 4, [fold.cursor, first.next_cursor!]), next);
  assert.equal(validateVisibleFold(next, selection, visibleContextWitness(page), fold, visiblePageCheckpoint(first), 4, [fold.cursor, first.next_cursor!]), next);
  for (const renderedBefore of [3, 5]) {
    assert.throws(() => validateVisibleFold(next, selection, visibleContextWitness(page), fold, visiblePageCheckpoint(first), renderedBefore), DocumentProjectionError);
  }
  const seed = structuredClone(page);
  seed.units[1].event!.seed_summary = true;
  assert.throws(() => validateVisibleUnits(seed, selection), DocumentProjectionError);
  const all: DocVisiblePage = { ...page, view: 'all', total: 11, to: 11, units: [{ from: 10, to: 11, turn_index: 0,
    event: null, fold: { from: 10, to: 11, rows: 0, cursor: 'empty' } }] };
  assert.equal(validateVisibleUnits(all, { ...selection, mode: 'all' }), all);
});

test('fold validation requires local contiguous raw ordinals, exact context/bounds and terminal rendered counts', () => {
  const context = heavyVisiblePage();
  const fold = context.units[0].fold!;
  const first = foldPage(context, 0, 0, 15);
  const next = foldPage(context, 0, 15, 30);
  assert.equal(validateVisibleFold(first, visibleSelection, context, fold), first);
  assert.equal(validateVisibleFold(next, visibleSelection, context, fold, first, 15), next);
  for (const invalid of [
    { ...next, kind: 'units' }, { ...next, from: 14 }, { ...next, from: 16 }, { ...next, fold: null },
    { ...next, fold: { from: 1, to: 30 } }, { ...next, units: context.units }, { ...next, events: null },
    { ...next, events: next.events.slice(1) }, { ...next, events: [...next.events].reverse() },
    { ...next, next_cursor: 'again' }, { ...next, events: next.events.map(event => ({ ...event, kind: 'reasoning', empty_reasoning: true })) },
    ...visibleContext(context).map((_value, index) => ({ ...next, [(['hash', 'base', 'generation', 'rules', 'view', 'start', 'total', 'inherited'] as const)[index]]: 'changed' })),
  ]) {
    assert.throws(() => validateVisibleFold(invalid, visibleSelection, context, fold, first, 15), DocumentProjectionError);
    assert.throws(() => validateVisibleFold(invalid, visibleSelection, visibleContextWitness(context), fold, visiblePageCheckpoint(first), 15), DocumentProjectionError);
  }
  assert.throws(() => validateVisibleFold({ ...first, next_cursor: fold.cursor }, visibleSelection, context, fold, undefined, 0, [fold.cursor]), DocumentProjectionError);
  assert.throws(() => validateVisibleFold({ ...first, next_cursor: fold.cursor }, visibleSelection, visibleContextWitness(context), fold, undefined, 0, [fold.cursor]), DocumentProjectionError);
  for (const previous of [{ ...first, kind: 'units' as const }, { ...first, next_cursor: null }, { ...first, fold: null },
    { ...first, fold: { from: 1, to: 30 } }, { ...first, generation: 'changed' }, { ...first, total: 1241 }]) {
    for (const checkpoint of [previous, visiblePageCheckpoint(previous)]) {
      assert.throws(() => validateVisibleFold(next, visibleSelection, visibleContextWitness(context), fold, checkpoint, 15), DocumentProjectionError);
    }
  }
  const largeContext: DocVisiblePage = { ...context, total: 201, to: 201, next_cursor: null,
    units: [{ from: 0, to: 201, turn_index: -1, event: visibleMessage(200, 'Large fold'), fold: { from: 0, to: 200, rows: 200, cursor: 'large' } }] };
  assert.equal(validateVisibleFold(foldPage(largeContext, 0, 0, 128), visibleSelection, largeContext, largeContext.units[0].fold!).events.length, 128);
  assert.throws(() => validateVisibleFold(foldPage(largeContext, 0), visibleSelection, largeContext, largeContext.units[0].fold!), DocumentProjectionError);
});

test('legacy checkpoints preserve range and total checks without retaining events or inventing a visible generation', () => {
  const first = { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' }, offset: 0, total: 2, inherited: 0, next: 1,
    events: [visibleMessage(0, 'Legacy initial')] };
  const legacy = validateLegacyDocument(first, visibleSelection, 0);
  const checkpoint = documentStreamCheckpoint(legacy);
  assert.equal(checkpoint.kind, 'legacy');
  if (checkpoint.kind !== 'legacy') throw new Error('Expected legacy checkpoint');
  const next = { ...first, offset: 1, next: -1, events: [visibleMessage(1, 'Legacy next')] };
  for (const previous of [legacy, checkpoint]) {
    assert.equal(validateLegacyDocument(next, visibleSelection, 1, previous).kind, 'legacy');
    for (const invalid of [first, { ...next, total: 3, next: 2 }, { ...next, hash: 'other' }, { ...next, offset: 2 },
      { ...next, next: 1 }, { ...next, projection: 'visible-v1' }, { ...next, kind: 'legacy' }, { ...next, units: [] },
      { ...next, events: [visibleMessage(0, 'Wrong ordinal')] }, { ...next, events: [{}] }]) {
      assert.throws(() => validateLegacyDocument(invalid, visibleSelection, 1, previous));
    }
    assert.throws(() => validateLegacyDocument(first, visibleSelection, 0, previous), DocumentProjectionError);
  }
});

test('fold query closures and stored page parameters are isolated from the original parent and fold payloads', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const context = clippedPromptPage();
  const fold = context.units[0].fold!;
  const first = clippedFoldPage(context, 10, 15);
  const next = clippedFoldPage(context, 15, 30);
  const bodies = objectReferences([context, first, next]);
  const options = documentFoldQueryOptions(client, { ...visibleSelection, mode: 'prompts' }, 'restart', context, fold);
  const requests: Record<string, string>[] = [];
  context.generation = 'mutated-parent';
  fold.from = 100;
  fold.to = 200;
  fold.rows = 100;
  fold.cursor = 'mutated-fold';
  for (const field of ['units', 'events', 'envelope']) {
    Object.defineProperty(context, field, { get: () => { throw new Error(`Retained parent ${field}`); } });
  }
  globalThis.fetch = async input => {
    const params = new URL(String(input), 'https://fixture.invalid').searchParams;
    requests.push(Object.fromEntries(params));
    return new Response(JSON.stringify(params.get('expand') === 'clipped-fold' ? first : next));
  };
  const observer = new InfiniteQueryObserver(client, options);
  try {
    assert.equal((await observer.fetchNextPage()).isError, false);
    const result = await observer.fetchNextPage();
    assert.equal(result.isError, false);
    assert.deepEqual(requests, [{ projection: 'visible-v1', expand: 'clipped-fold' }, { projection: 'visible-v1', expand: 'fold-next-15' }]);
    assert.equal(result.data!.pages.length, 2);
    const firstRecord = result.data!.pages[0];
    const expectedPosition = options.getNextPageParam(firstRecord, [firstRecord], options.initialPageParam, [options.initialPageParam]);
    assert.deepEqual(result.data!.pageParams[1], expectedPosition);
    assert.equal(expectedPosition?.renderedBefore, 4);
    assertBodyFree(result.data!.pageParams, bodies);
    assert.equal(result.hasNextPage, false);
  } finally {
    observer.destroy();
    client.clear();
    globalThis.fetch = original;
  }
});

test('terminal fold continuation errors stay disabled through invalidation and remount until explicit retry', async () => {
  const original = globalThis.fetch;
  const client = new QueryClient();
  const context = heavyVisiblePage();
  const fold = context.units[0].fold!;
  const first = foldPage(context, 0, 0, 15);
  const next = foldPage(context, 0, 15, 30);
  const options = documentFoldQueryOptions(client, visibleSelection, 'restart', context, fold);
  const enabled = options.enabled;
  assert.ok(typeof enabled === 'function');
  const requests: string[] = [];
  let rejected = false;
  globalThis.fetch = async input => {
    const cursor = new URL(String(input), 'https://fixture.invalid').searchParams.get('expand')!;
    requests.push(cursor);
    return rejected
      ? new Response(JSON.stringify({ error: { code: 'forbidden', message: 'Denied' } }), { status: 403 })
      : new Response(JSON.stringify(cursor === fold.cursor ? first : next));
  };
  const observer = new InfiniteQueryObserver(client, options);
  let unsubscribe = observer.subscribe(() => {});
  let remounted: typeof observer | undefined;
  try {
    assert.equal((await observer.refetch()).isError, false);
    rejected = true;
    const failed = await observer.fetchNextPage();
    assert.equal(failed.isFetchNextPageError, true);
    assert.deepEqual(requests, [fold.cursor, first.next_cursor]);
    assert.equal(enabled(observer.getCurrentQuery()), false);
    await client.invalidateQueries({ queryKey: options.queryKey });
    assert.deepEqual(requests, [fold.cursor, first.next_cursor]);
    unsubscribe();
    await client.invalidateQueries({ queryKey: options.queryKey });
    remounted = new InfiniteQueryObserver(client, documentFoldQueryOptions(client, visibleSelection, 'restart', context, fold));
    unsubscribe = remounted.subscribe(() => {});
    await client.invalidateQueries({ queryKey: options.queryKey });
    assert.deepEqual(requests, [fold.cursor, first.next_cursor]);
    assert.equal(remounted.getCurrentResult().isFetchNextPageError, true);
    rejected = false;
    const recovered = await remounted.fetchNextPage();
    assert.equal(recovered.isError, false);
    assert.deepEqual(requests, [fold.cursor, first.next_cursor, first.next_cursor]);
    assert.equal(enabled(remounted.getCurrentQuery()), true);
    assert.equal(recovered.data!.pages.length, 2);
  } finally {
    unsubscribe();
    observer.destroy();
    remounted?.destroy();
    client.clear();
    globalThis.fetch = original;
  }
});

test('terminal empty documents are valid, including fully inherited children', () => {
  for (const total of [0, 50]) {
    const page = { ...heavyVisiblePage(), total, inherited: total, start: total, from: total, to: total, units: [], next_cursor: null };
    assert.equal(validateVisibleUnits(page, visibleSelection), page);
    assert.throws(() => validateVisibleUnits({ ...page, next_cursor: 'again' }, visibleSelection));
  }
});

test('compatible read helpers snapshot full-page arguments before asynchronous requests complete', async () => {
  const original = globalThis.fetch;
  const signal = new AbortController().signal;
  const previous = heavyVisiblePage();
  const next = heavyVisiblePage(620);
  let body: unknown = next;
  globalThis.fetch = async () => new Response(JSON.stringify(body));
  try {
    const reading = readDocumentStream(visibleSelection, signal, previous, [previous.next_cursor!]);
    previous.generation = 'mutated';
    previous.to = 1000;
    previous.units.length = 0;
    assert.deepEqual(await reading, next);

    const context = heavyVisiblePage();
    const fold = context.units[0].fold!;
    const firstFold = foldPage(context, 0, 0, 15);
    const nextFold = foldPage(context, 0, 15, 30);
    body = nextFold;
    const folding = readDocumentFold(visibleSelection, signal, context, fold, firstFold, 15, [fold.cursor]);
    context.generation = 'mutated';
    fold.rows = 1;
    fold.to = 1;
    firstFold.to = 1;
    firstFold.fold!.from = 1;
    firstFold.events.length = 0;
    assert.deepEqual(await folding, nextFold);

    const legacy = validateLegacyDocument({ hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' },
      offset: 0, total: 2, inherited: 0, next: 1, events: [visibleMessage(0, 'Legacy initial')] }, visibleSelection, 0);
    const nextLegacy = { ...legacy, offset: 1, next: -1, events: [visibleMessage(1, 'Legacy next')] };
    const { kind: _kind, ...legacyWire } = nextLegacy;
    body = legacyWire;
    const legacyReading = readDocumentStream(visibleSelection, signal, legacy);
    legacy.next = -1;
    legacy.total = 100;
    legacy.events.length = 0;
    assert.deepEqual(await legacyReading, nextLegacy);
  } finally { globalThis.fetch = original; }
});

test('projection transport falls back only on explicit unsupported or valid untagged initial; continuations never mix', async () => {
  const original = globalThis.fetch;
  const requests: { url: URL; signal?: AbortSignal | null; credentials?: RequestCredentials }[] = [];
  const legacy = { hash: 'document', envelope: { cir_version: '2', source_provider: 'codex' }, offset: 0, total: 2, inherited: 0, next: 1,
    events: [visibleMessage(0, 'Legacy initial')] };
  let body: unknown = legacy;
  let status = 200;
  let unsupported = false;
  globalThis.fetch = async (input, init) => {
    const url = new URL(String(input), 'https://fixture.invalid');
    requests.push({ url, signal: init?.signal, credentials: init?.credentials });
    if (unsupported && url.searchParams.has('projection')) return new Response(JSON.stringify({ error: { code: 'doc_visible_projection_unsupported' } }), { status: 409 });
    return new Response(JSON.stringify(body), { status });
  };
  const controller = new AbortController();
  try {
    const initial = await readDocumentStream(visibleSelection, controller.signal);
    assert.equal(initial.kind, 'legacy');
    assert.equal(requests.length, 1);
    assert.equal(requests[0].credentials, 'include');
    assert.equal(requests[0].signal, controller.signal);
    body = { ...legacy, offset: 1, next: -1, events: [visibleMessage(1, 'Legacy next')] };
    for (const previous of [initial, documentStreamCheckpoint(initial)]) {
      assert.equal((await readDocumentStream(visibleSelection, controller.signal, previous)).kind, 'legacy');
      assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { view: 'chat', offset: '1', limit: '20' });
    }
    body = legacy;
    unsupported = true;
    const before = requests.length;
    assert.equal((await readDocumentStream(visibleSelection, controller.signal)).kind, 'legacy');
    assert.equal(requests.length, before + 2);
    await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal, heavyVisiblePage()), ApiError);
    await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal, visiblePageCheckpoint(heavyVisiblePage())), ApiError);
    unsupported = false;
    for (const invalid of [{ ...legacy, projection: null }, { ...legacy, events: null }, { ...legacy, events: [visibleMessage(1, 'sparse')] }, { ...legacy, kind: 'units' }]) {
      body = invalid;
      await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal));
    }
    body = legacy;
    await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal, heavyVisiblePage()), DocumentProjectionError);
    await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal, visiblePageCheckpoint(heavyVisiblePage())), DocumentProjectionError);
    body = heavyVisiblePage(620);
    for (const previous of [heavyVisiblePage(), visiblePageCheckpoint(heavyVisiblePage())]) {
      await readDocumentStream(visibleSelection, controller.signal, previous, ['main-620']);
      assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { projection: 'visible-v1', cursor: 'main-620' });
    }
    body = foldPage(heavyVisiblePage(), 0);
    await readDocumentFold(visibleSelection, controller.signal, heavyVisiblePage(), heavyVisiblePage().units[0].fold!);
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { projection: 'visible-v1', expand: 'fold-0' });
    for (const failure of [{ status: 401, code: 'doc_visible_projection_unsupported' }, { status: 403, code: 'doc_visible_projection_unsupported' },
      { status: 409, code: 'doc_visible_generation_stale' }, { status: 503, code: 'doc_range_index_pending' }, { status: 422, code: 'integrity' }]) {
      status = failure.status;
      body = { error: { code: failure.code, message: 'Rejected' } };
      const count: number = requests.length;
      await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal), ApiError);
      assert.equal(requests.length, count + 1);
      const continued: number = requests.length;
      await assert.rejects(() => readDocumentStream(visibleSelection, controller.signal, visiblePageCheckpoint(heavyVisiblePage())), ApiError);
      assert.equal(requests.length, continued + 1);
    }
  } finally { globalThis.fetch = original; }
});
