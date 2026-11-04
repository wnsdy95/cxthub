import assert from 'node:assert/strict';
import { test } from 'node:test';
import { documentFindParameters, documentFindFragmentParameters, validateDocumentFindFragment, validateDocumentFindPage, type DocFindMatch, type DocFindPage,
  type DocumentFindPosition, type DocumentFindSource, type LegacyDocumentFindSource, type RootDocumentFindSource } from '../src/documentFind';
import { DocumentProjectionError } from '../src/documentVisible';

const source: DocumentFindSource = {
  id: 'root',
  selection: { repoId: 'repo', hash: 'document', base: 'base', start: -1, mode: 'chat' },
  context: { hash: 'document', base: 'base', generation: 'generation', rules: 1, view: 'chat', start: 2, total: 40, inherited: 2 },
  replay: 'opaque_replay-token',
  from: 2,
  to: 30,
};

const legacySource: LegacyDocumentFindSource = {
  id: 'legacy',
  protocol: 'legacy',
  selection: { ...source.selection },
  context: { hash: 'document', base: 'base', view: 'chat', start: 2, total: 40, inherited: 2 },
  from: 2,
  to: 30,
};

const documentBytes = 512 * 1024 * 1024;

test('verified source ranges are explicit, bounded and exact for raw matches', () => {
  const current = page({ matches: [match({ source_range: { from: 120_000, to: 120_006 } })] });
  assert.equal(validateDocumentFindPage(current, source), current);
  for (const range of [null, {}, { from: -1, to: 4 }, { from: 3, to: 3 }, { from: 4, to: documentBytes + 1 },
    { from: 1.5, to: 4 }, { from: 0, to: 4, extra: 1 }]) {
    assert.throws(() => validateDocumentFindPage(page({ matches: [match({ source_range: range as DocFindMatch['source_range'] })] }), source), DocumentProjectionError);
  }
  assert.throws(() => validateDocumentFindPage(page({ matches: [match({ raw: true, source_range: { from: 4, to: 9 } })] }), source), DocumentProjectionError);
});

test('matched fragments bind immutable source and exact generation without translating selectors', () => {
  const selected = match({ source_range: { from: 120_000, to: 120_006 } });
  assert.deepEqual(Object.fromEntries(documentFindFragmentParameters(source, selected, 120_000, 64 * 1024)), {
    selection: selected.reveal_cursor, offset: '120000', budget: '65536',
  });
  const fragment = { projection: 'find-fragment-v1', hash: 'document', generation: 'generation', event_index: 2,
    byte_offset: 120_000, total_bytes: 130_000, json_fragment: 'NeedLE🙂', next: 120_010, previous: 54_464 };
  assert.equal(validateDocumentFindFragment(fragment, source, selected, 120_000, 64 * 1024), fragment);
  for (const change of [{ projection: 'find-legacy-fragment-v1' }, { generation: '' }, { hash: 'other' }, { event_index: 3 },
    { byte_offset: 0 }, { next: 120_009 }, { previous: 120_000 }, { previous: -1 }, { total_bytes: 120_001 },
    { json_fragment: '\ud800' }, { json_fragment: '' }, { json_fragment: 'x'.repeat(65_537) }, { extra: true }]) {
    assert.throws(() => validateDocumentFindFragment({ ...fragment, ...change }, source, selected, 120_000, 64 * 1024), DocumentProjectionError);
  }
  for (const range of [{ from: 120_000, to: 120_008 }, { from: 120_007, to: 120_010 }]) {
    assert.throws(() => validateDocumentFindFragment(fragment, source, { ...selected, source_range: range }, 120_000, 64 * 1024), DocumentProjectionError);
  }
  const legacyMatch = { ...selected, reveal_cursor: '' };
  assert.deepEqual(Object.fromEntries(documentFindFragmentParameters(legacySource, legacyMatch, 120_000, 64 * 1024)), {
    legacy: '1', index: '2', total: '40', offset: '120000', budget: '65536',
  });
  const legacyFragment = { ...fragment, projection: 'find-legacy-fragment-v1', generation: '' };
  assert.equal(validateDocumentFindFragment(legacyFragment, legacySource, legacyMatch, 120_000, 64 * 1024), legacyFragment);
  assert.throws(() => validateDocumentFindFragment(fragment, legacySource, legacyMatch, 120_000, 64 * 1024), DocumentProjectionError);
  const final = { ...fragment, byte_offset: 0, total_bytes: 10, next: -1, previous: -1 };
  assert.equal(validateDocumentFindFragment(final, source, match(), 0, 64 * 1024), final);
});
const findFields = 128 * 1024;
const bytes = (text: string) => new TextEncoder().encode(text).length;

function match(change: Partial<DocFindMatch> = {}): DocFindMatch {
  return { event_index: 2, field: 0, byte_offset: 4, byte_length: 6, raw: false, preview: '\uc55e NeedLE🙂 \ub4a4',
    preview_byte_offset: 0, reveal_cursor: 'opaque_reveal-token', ...change };
}

function page(change: Partial<DocFindPage> = {}): DocFindPage {
  return { projection: 'find-v1', hash: 'document', generation: 'generation', from: 2, to: 30,
    event_index: 3, field: 0, byte_offset: 0, matches: [match()], next_cursor: 'opaque_next-token', ...change };
}

function invalid(value: unknown, previous?: DocumentFindPosition['frontier']) {
  assert.throws(() => validateDocumentFindPage(value, source, previous), DocumentProjectionError);
}

function legacyMatch(change: Partial<DocFindMatch> = {}): DocFindMatch {
  return match({ reveal_cursor: '', ...change });
}

function legacyPage(change: Partial<DocFindPage> = {}): DocFindPage {
  return page({ projection: 'find-legacy-v1', generation: '', matches: [legacyMatch()], ...change });
}

function invalidLegacy(value: unknown, previous?: DocumentFindPosition['frontier']) {
  assert.throws(() => validateDocumentFindPage(value, legacySource, previous), DocumentProjectionError);
}

test('find parameters use only the actual initial and continuation wire keys and preserve opaque tokens', () => {
  const before = structuredClone(source);
  const query = ' "Needle" + & ?\n\ud55c🙂 ';
  assert.deepEqual([...documentFindParameters(query, source)], [
    ['q', query], ['selection', source.replay], ['from', '2'], ['to', '30'],
  ]);
  assert.deepEqual([...documentFindParameters(query, source, { source: 0 })], [...documentFindParameters(query, source)]);
  const position: DocumentFindPosition = { source: 7, cursor: 'not_decoded-or-reencoded', frontier: { index: 5, field: 2, byte: 30 } };
  const originalPosition = structuredClone(position);
  const parameters = documentFindParameters(query, source, position);
  assert.deepEqual([...parameters], [['q', query], ['cursor', position.cursor]]);
  assert.deepEqual([...new URLSearchParams(parameters.toString())], [...parameters]);
  assert.deepEqual(source, before);
  assert.deepEqual(position, originalPosition);
});

test('find query validation counts Unicode scalars without trimming or normalizing them', () => {
  for (const query of [' ', '\n', '\u0000', 'e\u0301', '🙂'.repeat(256), '\ud55c'.repeat(256)]) {
    assert.equal(documentFindParameters(query, source).get('q'), query);
  }
  for (const query of ['', '🙂'.repeat(257), 'a'.repeat(257), '\ud800', '\udfff', 'a\ud800b', '\ud800\ud800', undefined, null, 7]) {
    assert.throws(() => documentFindParameters(query as string, source), DocumentProjectionError);
  }
});

test('find requests reject missing, oversized and non-base64url selectors without decoding valid-shaped tokens', () => {
  for (const replay of ['', 'a'.repeat(2732), 'bad=', 'bad+', 'bad/', 'bad token', '\n', '\ud55c', null, undefined, 5]) {
    assert.throws(() => documentFindParameters('needle', { ...source, replay } as DocumentFindSource), DocumentProjectionError);
  }
  for (const cursor of ['', 'a'.repeat(5463), 'bad=', 'bad+', 'bad/', 'bad token', 'bad\n', '🙂', null, 5]) {
    assert.throws(() => documentFindParameters('needle', source, { source: 0, cursor } as DocumentFindPosition), DocumentProjectionError);
  }
  assert.equal(documentFindParameters('needle', { ...source, replay: 'a'.repeat(2731) }).get('selection')?.length, 2731);
  assert.equal(documentFindParameters('needle', source, { source: 0, cursor: 'a'.repeat(5462) }).get('cursor')?.length, 5462);
  for (const position of [{ source: -1 }, { source: 0.5 }, { source: NaN }, { source: Number.MAX_SAFE_INTEGER + 1 },
    { source: 0, frontier: { index: 1, field: 0, byte: 0 } }, { source: 0, frontier: { index: 30, field: 1, byte: 0 } }]) {
    assert.throws(() => documentFindParameters('needle', source, position), DocumentProjectionError);
  }
});

test('source admission requires an existing visible generation and the selected range with no legacy fallback', () => {
  const changes: Partial<DocumentFindSource>[] = [
    { context: undefined }, { context: { ...source.context, generation: '' } },
    { context: { ...source.context, generation: undefined } as unknown as DocumentFindSource['context'] },
    { context: { ...source.context, hash: 'other' } }, { context: { ...source.context, base: 'other' } },
    { context: { ...source.context, view: 'prompts' } }, { context: { ...source.context, rules: 2 } },
    { context: { ...source.context, start: 3 } }, { context: { ...source.context, total: documentBytes + 1 } },
    { context: { ...source.context, total: -1 } }, { context: { ...source.context, inherited: 41 } },
    { selection: { ...source.selection, start: 3 } }, { selection: { ...source.selection, start: -2 } },
    { from: 1 }, { from: 31 }, { from: 2.5 }, { from: NaN }, { to: 41 }, { to: -1 }, { to: Infinity },
  ];
  for (const change of changes) {
    const changed = { ...source, ...change } as DocumentFindSource;
    assert.throws(() => documentFindParameters('needle', changed), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(page(), changed), DocumentProjectionError);
  }
  const root: DocumentFindSource = { ...source, id: 'root-without-base', from: 0,
    selection: { ...source.selection, base: undefined, start: 0 }, context: { ...source.context, base: '', start: 0, inherited: 0 } };
  const composed: DocumentFindSource = { ...source, id: 'composed', from: 10, to: 40,
    selection: { ...source.selection, start: 10 }, context: { ...source.context, start: 10 } };
  for (const current of [root, composed]) {
    const terminal = page({ from: current.from, to: current.to, event_index: current.to, matches: [], next_cursor: null });
    assert.equal(validateDocumentFindPage(terminal, current), terminal);
  }
});

test('find pages preserve the exact Go wire object including raw coordinates and Unicode previews', () => {
  const current = page({ matches: [Object.freeze(match()), Object.freeze(match({ event_index: 2, field: 1, raw: true,
    byte_offset: 1007, preview_byte_offset: 1000, byte_length: 3, preview: '\uc55e🙂K \ub4a4' }))] });
  Object.freeze(current.matches);
  Object.freeze(current);
  assert.equal(validateDocumentFindPage(current, source), current);
  assert.equal(current.matches[1].raw, true);
  const terminal = page({ event_index: source.to, next_cursor: null });
  assert.equal(validateDocumentFindPage(terminal, source), terminal);
  const emptySource = { ...source, from: source.to };
  const empty = page({ from: source.to, event_index: source.to, matches: [], next_cursor: null });
  assert.equal(validateDocumentFindPage(empty, emptySource), empty);
  assert.equal(validateDocumentFindPage(empty, emptySource, { index: source.to, field: 0, byte: 0 }), empty);
});

test('find pages require every current wire key and reject unknown keys, null lists and legacy envelopes', () => {
  for (const value of [undefined, null, [], {}, 'find-v1', 5, { ...page(), projection: 'visible-v1' }, { ...page(), projection: undefined },
    { ...page(), hash: 'other' }, { ...page(), generation: 'other' }, { ...page(), generation: '' }, { ...page(), from: 1 },
    { ...page(), to: 31 }, { ...page(), matches: null }, { ...page(), matches: {} }, { ...page(), matches: [null] },
    { ...page(), matches: Array(1) }, { ...page(), unexpected: true }, { ...page(), units: [] },
    { hash: 'document', envelope: {}, events: [], total: 40, offset: 2, next: 3, inherited: 2 }]) invalid(value);
  for (const key of Object.keys(page())) {
    const missing: Record<string, unknown> = { ...page() };
    delete missing[key];
    invalid(missing);
    const inherited = Object.assign(Object.create({ [key]: page()[key as keyof DocFindPage] }), missing);
    invalid(inherited);
  }
  for (const key of Object.keys(match())) {
    const missing: Record<string, unknown> = { ...match() };
    delete missing[key];
    invalid(page({ matches: [missing as unknown as DocFindMatch] }));
  }
  invalid(page({ matches: [{ ...match(), unknown: 'payload' } as DocFindMatch] }));
});

test('find frontier progress is lexicographic, allows bounded empty scans and rejects stalls or terminal inconsistencies', () => {
  const previous = { index: 4, field: 3, byte: 12 };
  for (const change of [{ event_index: 4, field: 3, byte_offset: 13 }, { event_index: 4, field: 4, byte_offset: 0 },
    { event_index: 5, field: 0, byte_offset: 0 }, { event_index: source.to, field: 0, byte_offset: 0, next_cursor: null }]) {
    const current = page({ matches: [], ...change });
    assert.equal(validateDocumentFindPage(current, source, previous), current);
  }
  for (const change of [{ event_index: 4, field: 3, byte_offset: 12 }, { event_index: 4, field: 3, byte_offset: 11 },
    { event_index: 4, field: 2, byte_offset: 100 }, { event_index: 3, field: 4, byte_offset: 100 },
    { event_index: source.to, field: 1, byte_offset: 0, next_cursor: null },
    { event_index: source.to, field: 0, byte_offset: 1, next_cursor: null },
    { event_index: source.to }, { event_index: 5, next_cursor: null }]) invalid(page({ matches: [], ...change }), previous);
  invalid(page({ event_index: source.from, matches: [] }));
  for (const prior of [{ index: 1, field: 0, byte: 0 }, { index: 31, field: 0, byte: 0 }, { index: 3, field: -1, byte: 0 },
    { index: 3, field: 0, byte: 0.5 }, { index: source.to, field: 0, byte: 1 }, { index: 3, field: findFields + 1, byte: 0 },
    { index: 3, field: 0, byte: documentBytes + 1 }]) invalid(page({ matches: [] }), prior);
});

test('frontier and match ordinals use bounded safe integers instead of coercion', () => {
  for (const value of [-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '3', null, undefined]) {
    for (const key of ['event_index', 'field', 'byte_offset']) invalid({ ...page(), [key]: value });
    for (const key of ['event_index', 'field', 'byte_offset', 'byte_length', 'preview_byte_offset']) {
      invalid(page({ matches: [{ ...match(), [key]: value }] }));
    }
  }
  for (const change of [{ event_index: source.from - 1 }, { event_index: source.to + 1 }, { field: findFields + 1 },
    { byte_offset: documentBytes + 1 }]) invalid(page(change));
  for (const change of [{ event_index: source.from - 1 }, { event_index: source.to }, { field: findFields + 1 },
    { byte_length: 0 }, { byte_length: 1025 }, { byte_offset: documentBytes, preview_byte_offset: documentBytes },
    { preview_byte_offset: 5 }]) invalid(page({ matches: [match(change)] }));
  for (const raw of [null, undefined, 0, 1, 'false', {}, []]) invalid(page({ matches: [{ ...match(), raw } as DocFindMatch] }));
  const maximum = page({ field: findFields, byte_offset: documentBytes, matches: [] });
  assert.equal(validateDocumentFindPage(maximum, source), maximum);
  const nearEnd = match({ field: findFields, byte_offset: documentBytes - 1, byte_length: 1, preview: 'x', preview_byte_offset: documentBytes - 1 });
  const terminal = page({ event_index: source.to, matches: [nearEnd], next_cursor: null });
  assert.equal(validateDocumentFindPage(terminal, source), terminal);
});

test('match lists are capped, strictly sorted and bounded by the inclusive prior and exclusive new frontier', () => {
  const matches = Array.from({ length: 20 }, (_, index) => match({ byte_offset: index, byte_length: 1, preview: 'a', preview_byte_offset: index }));
  const full = page({ matches, event_index: source.from, byte_offset: 20 });
  assert.equal(validateDocumentFindPage(full, source), full);
  invalid(page({ matches: [...matches, match({ event_index: 3 })], event_index: 4 }));
  invalid(page({ matches: [match(), match()] }));
  invalid(page({ matches: [...matches].reverse() }));
  invalid(page({ matches: [match({ event_index: 3 }), match({ event_index: 2 })], event_index: 4 }));
  invalid(page({ matches: [match({ field: 1 }), match({ field: 0 })] }));
  invalid(page({ matches: [match({ event_index: 3 })] }));
  invalid(page({ matches: [match({ event_index: 4 })] }));
  invalid(page({ event_index: 2, byte_offset: 4 }));
  invalid(page({ event_index: 2, byte_offset: 3 }));
  const prior = { index: 2, field: 0, byte: 4 };
  const overlap = page({ event_index: 2, byte_offset: 5 });
  assert.equal(validateDocumentFindPage(overlap, source, prior), overlap);
  invalid(overlap, { ...prior, byte: 5 });
  const overlappingMatches = page({ event_index: 2, byte_offset: 4, matches: [
    match({ byte_offset: 1, byte_length: 3, preview: 'banana' }), match({ byte_offset: 3, byte_length: 3, preview: 'banana' }),
  ] });
  assert.equal(validateDocumentFindPage(overlappingMatches, source), overlappingMatches);
});

test('find and reveal cursors stay opaque, nonempty, base64url-shaped and separately bounded', () => {
  for (const next_cursor of ['', undefined, 0, {}, 'a'.repeat(5463), 'bad=', 'bad+', 'bad/', 'bad token', 'bad\n', '\ud55c']) {
    invalid({ ...page(), next_cursor });
  }
  for (const reveal_cursor of ['', undefined, null, 0, {}, 'a'.repeat(2732), 'bad=', 'bad+', 'bad/', 'bad token', 'bad\n', '🙂']) {
    invalid(page({ matches: [{ ...match(), reveal_cursor } as DocFindMatch] }));
  }
  const maximum = page({ next_cursor: 'a'.repeat(5462), matches: [match({ reveal_cursor: 'z'.repeat(2731) })] });
  assert.equal(validateDocumentFindPage(maximum, source), maximum);
  const unchanged = page({ next_cursor: source.replay, matches: [match({ reveal_cursor: source.replay })] });
  assert.equal(validateDocumentFindPage(unchanged, source), unchanged);
});

test('preview spans use UTF-8 byte boundaries rather than UTF-16 or character offsets', () => {
  const preview = 'aé\ud55c🙂z';
  for (const [byte_offset, byte_length] of [[0, 1], [1, 2], [3, 3], [6, 4], [10, 1], [1, 9]]) {
    const current = page({ matches: [match({ preview, byte_offset, byte_length })] });
    assert.equal(validateDocumentFindPage(current, source), current);
  }
  for (const [byte_offset, byte_length] of [[2, 1], [4, 2], [7, 3], [1, 1], [3, 1], [6, 3], [11, 1], [0, 12]]) {
    invalid(page({ matches: [match({ preview, byte_offset, byte_length })] }));
  }
  for (const previewValue of ['', null, undefined, 7, [], '\ud800', '\udfff', 'a\ud800b', 'a\udfff🙂', 'a\ud800\ud800']) {
    invalid(page({ matches: [match({ preview: previewValue as string, byte_offset: 0, byte_length: 1 })] }));
  }
  const shifted = page({ matches: [match({ preview, preview_byte_offset: 123, byte_offset: 129, byte_length: 4, raw: true })] });
  assert.equal(validateDocumentFindPage(shifted, source), shifted);
  invalid(page({ matches: [match({ preview: 'aa', preview_byte_offset: documentBytes - 1, byte_offset: documentBytes - 1, byte_length: 1 })] }));
});

test('preview and match budgets count runes with at most 32 context runes on each side', () => {
  const prefix = '🙂'.repeat(32);
  const matched = '\ud55c'.repeat(256);
  const suffix = 'é'.repeat(32);
  const preview = prefix + matched + suffix;
  const maximum = page({ matches: [match({ preview, byte_offset: bytes(prefix), byte_length: bytes(matched) })] });
  assert.equal(validateDocumentFindPage(maximum, source), maximum);
  const astral = page({ matches: [match({ preview: '🙂'.repeat(320), byte_offset: 128, byte_length: 1024 })] });
  assert.equal(validateDocumentFindPage(astral, source), astral);
  for (const change of [
    { preview: prefix + matched + suffix + 'a', byte_offset: bytes(prefix), byte_length: bytes(matched) },
    { preview: '🙂'.repeat(321), byte_offset: 128, byte_length: 1024 },
    { preview: 'a'.repeat(257), byte_offset: 0, byte_length: 257 },
    { preview: 'é'.repeat(257), byte_offset: 0, byte_length: 514 },
    { preview: 'a'.repeat(33) + 'x', byte_offset: 33, byte_length: 1 },
    { preview: 'x' + 'a'.repeat(33), byte_offset: 0, byte_length: 1 },
    { preview: 'a'.repeat(100_000), byte_offset: 0, byte_length: 1 },
  ]) invalid(page({ matches: [match(change)] }));
  const controls = page({ matches: [match({ preview: '\u0000\n\ufffd', byte_offset: 0, byte_length: 2 })] });
  assert.equal(validateDocumentFindPage(controls, source), controls);
});

test('legacy parameters contain exactly the admitted bounds and optional nonempty base without visible selectors', () => {
  const before = structuredClone(legacySource);
  const query = ' "Needle" + & ?\n\ud55c🙂 ';
  const expected = [['q', query], ['legacy', '1'], ['from', '2'], ['to', '30'], ['total', '40'], ['inherited', '2'], ['base', 'base']];
  assert.deepEqual([...documentFindParameters(query, legacySource)], expected);
  assert.deepEqual([...documentFindParameters(query, legacySource, { source: 3 })], expected);
  const position: DocumentFindPosition = { source: 7, cursor: 'not_decoded-or-reencoded', frontier: { index: 5, field: 2, byte: 30 } };
  const originalPosition = structuredClone(position);
  const parameters = documentFindParameters(query, legacySource, position);
  assert.deepEqual([...parameters], [['q', query], ['legacy', '1'], ['cursor', position.cursor]]);
  assert.deepEqual([...new URLSearchParams(parameters.toString())], [...parameters]);
  const noBase: LegacyDocumentFindSource = { ...legacySource, from: 0,
    selection: { ...legacySource.selection, base: undefined }, context: { ...legacySource.context, base: '', start: 0, inherited: 0 } };
  assert.deepEqual([...documentFindParameters(query, noBase)], [
    ['q', query], ['legacy', '1'], ['from', '0'], ['to', '30'], ['total', '40'], ['inherited', '0'],
  ]);
  assert.deepEqual([...documentFindParameters(query, { ...noBase, selection: { ...noBase.selection, base: '' } })],
    [...documentFindParameters(query, noBase)]);
  assert.deepEqual([...documentFindParameters(query, noBase, position)], [['q', query], ['legacy', '1'], ['cursor', position.cursor]]);
  assert.deepEqual(legacySource, before);
  assert.deepEqual(position, originalPosition);
});

test('legacy queries preserve the same unnormalized 1 through 256 Unicode scalar contract', () => {
  for (const query of [' ', '\n', '\u0000', 'e\u0301', '🙂'.repeat(256), '\ud55c'.repeat(256)]) {
    assert.equal(documentFindParameters(query, legacySource).get('q'), query);
    assert.equal(documentFindParameters(query, legacySource, { source: 0, cursor: 'opaque' }).get('q'), query);
  }
  for (const query of ['', '🙂'.repeat(257), 'a'.repeat(257), '\ud800', '\udfff', 'a\ud800b', '\ud800\ud800', undefined, null, 7]) {
    assert.throws(() => documentFindParameters(query as string, legacySource), DocumentProjectionError);
    assert.throws(() => documentFindParameters(query as string, legacySource, { source: 0, cursor: 'opaque' }), DocumentProjectionError);
  }
});

test('legacy admission rejects invalid context, selection and bounds even for continuation requests', () => {
  const changes: Record<string, unknown>[] = [
    { context: undefined }, { context: null }, { context: [] }, { selection: undefined }, { selection: null },
    { context: { ...legacySource.context, hash: 'other' } }, { context: { ...legacySource.context, base: 'other' } },
    { context: { ...legacySource.context, base: undefined } }, { context: { ...legacySource.context, view: 'prompts' } },
    { context: { ...legacySource.context, start: 3 } }, { context: { ...legacySource.context, total: documentBytes + 1 } },
    { context: { ...legacySource.context, inherited: 41 } }, { context: { ...legacySource.context, start: 41 } },
    { selection: { ...legacySource.selection, hash: '' }, context: { ...legacySource.context, hash: '' } },
    { selection: { ...legacySource.selection, hash: undefined }, context: { ...legacySource.context, hash: undefined } },
    { selection: { ...legacySource.selection, base: 5 }, context: { ...legacySource.context, base: 5 } },
    { selection: { ...legacySource.selection, mode: 'other' }, context: { ...legacySource.context, view: 'other' } },
    { selection: { ...legacySource.selection, start: 3 } }, { selection: { ...legacySource.selection, start: -2 } },
    { selection: { ...legacySource.selection, start: 41 }, context: { ...legacySource.context, start: 41 } },
    { selection: { ...legacySource.selection, base: undefined }, context: { ...legacySource.context, base: '' } },
    { from: 1 }, { from: 31 }, { to: 41 },
  ];
  const invalidOrdinalValues = [-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '2', null, undefined];
  for (const value of invalidOrdinalValues) {
    for (const key of ['total', 'inherited', 'start']) changes.push({ context: { ...legacySource.context, [key]: value } });
    for (const key of ['from', 'to']) changes.push({ [key]: value });
    if (value !== -1) changes.push({ selection: { ...legacySource.selection, start: value } });
  }
  for (const value of [null, undefined, [], ...changes.map(change => ({ ...legacySource, ...change }))]) {
    const changed = value as unknown as DocumentFindSource;
    assert.throws(() => documentFindParameters('needle', changed), DocumentProjectionError);
    assert.throws(() => documentFindParameters('needle', changed, { source: 0, cursor: 'opaque' }), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(legacyPage(), changed), DocumentProjectionError);
  }
});

test('legacy admission uses the selected start or inherited boundary and permits bounded empty ranges', () => {
  const noBase: LegacyDocumentFindSource = { ...legacySource, from: 0,
    selection: { ...legacySource.selection, base: undefined }, context: { ...legacySource.context, base: '', start: 0, inherited: 0 } };
  const selected: LegacyDocumentFindSource = { ...legacySource, from: 10, to: 40,
    selection: { ...legacySource.selection, start: 10 }, context: { ...legacySource.context, start: 10 } };
  const beforeInherited: LegacyDocumentFindSource = { ...legacySource, from: 1,
    selection: { ...legacySource.selection, start: 1 }, context: { ...legacySource.context, start: 1 } };
  const maximum: LegacyDocumentFindSource = { ...legacySource, from: documentBytes, to: documentBytes,
    context: { ...legacySource.context, total: documentBytes, inherited: documentBytes, start: documentBytes } };
  const empty: LegacyDocumentFindSource = { ...noBase, to: 0, context: { ...noBase.context, total: 0 } };
  const emptyRange: LegacyDocumentFindSource = { ...legacySource, from: legacySource.to };
  for (const mode of ['all', 'chat', 'prompts'] as const) {
    for (const current of [noBase, selected, beforeInherited, maximum, empty, emptyRange]) {
      const admitted = { ...current, selection: { ...current.selection, mode }, context: { ...current.context, view: mode } };
      const terminal = legacyPage({ from: current.from, to: current.to, event_index: current.to, matches: [], next_cursor: null });
      assert.equal(validateDocumentFindPage(terminal, admitted), terminal);
      assert.equal(documentFindParameters('needle', admitted).get('from'), String(current.from));
      assert.equal(documentFindParameters('needle', admitted).get('to'), String(current.to));
    }
  }
});

test('legacy sources never borrow visible generations, rules or replay selectors and root sources cannot downgrade', () => {
  const polluted: unknown[] = [];
  for (const key of ['generation', 'rules']) {
    for (const value of ['generation', '', 1, 0, null, undefined]) {
      polluted.push({ ...legacySource, context: { ...legacySource.context, [key]: value } });
      polluted.push({ ...legacySource, context: Object.assign(Object.create({ [key]: value }), legacySource.context) });
    }
  }
  for (const replay of ['opaque', '', null, undefined]) polluted.push({ ...legacySource, replay });
  polluted.push(Object.assign(Object.create({ replay: 'opaque' }), legacySource));
  const { protocol, ...untaggedLegacy } = legacySource;
  assert.equal(protocol, 'legacy');
  const { replay, ...rootWithoutReplay } = source;
  assert.equal(replay, source.replay);
  for (const changed of [...polluted, untaggedLegacy, { ...untaggedLegacy, protocol: 'visible-v1' },
    { ...source, protocol: 'legacy' }, { ...rootWithoutReplay, protocol: 'legacy' },
    { ...source, context: { ...source.context, generation: '' } }, { ...source, context: { ...source.context, rules: undefined } },
    ...[null, '', 'find-v1', 'find-legacy-v1', 1].flatMap(protocol => [{ ...source, protocol }, { ...legacySource, protocol }])]) {
    assert.throws(() => documentFindParameters('needle', changed as DocumentFindSource), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(legacyPage(), changed as DocumentFindSource), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(page(), changed as DocumentFindSource), DocumentProjectionError);
  }
  const taggedRoot: RootDocumentFindSource = { ...source, protocol: 'visible-v1' };
  assert.deepEqual([...documentFindParameters('needle', taggedRoot)], [...documentFindParameters('needle', source)]);
  const current = page();
  assert.equal(validateDocumentFindPage(current, taggedRoot), current);
});

test('legacy pages require their own projection and explicit empty generation and never cross root protocols', () => {
  const current = legacyPage({ matches: [Object.freeze(legacyMatch()), Object.freeze(legacyMatch({ field: 1, raw: true,
    byte_offset: 1007, preview_byte_offset: 1000, byte_length: 3, preview: '\uc55e🙂K \ub4a4' }))] });
  Object.freeze(current.matches);
  Object.freeze(current);
  assert.equal(validateDocumentFindPage(current, legacySource), current);
  invalid(current);
  invalidLegacy(page());
  for (const generation of ['generation', 'legacy', 'fake-generation', undefined, null, 0]) invalidLegacy({ ...current, generation });
  for (const projection of ['find-v1', 'visible-v1', 'legacy', undefined, null]) invalidLegacy({ ...current, projection });
  for (const root of [source, { ...source, protocol: 'visible-v1' as const }]) {
    assert.throws(() => validateDocumentFindPage(legacyPage({ matches: [] }), root), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(page({ generation: '', matches: [] }), root), DocumentProjectionError);
    assert.throws(() => validateDocumentFindPage(legacyPage({ generation: source.context.generation, matches: [] }), root), DocumentProjectionError);
  }
  invalidLegacy(page({ generation: '', matches: [] }));
  invalidLegacy(legacyPage({ generation: source.context.generation, matches: [] }));
});

test('legacy match reveal cursors must be exactly empty while root matches still require visible opaque selectors', () => {
  for (const reveal_cursor of ['opaque_reveal-token', 'a'.repeat(2731), 'bad=', ' ', '\n', undefined, null, 0, false, {}, []]) {
    invalidLegacy(legacyPage({ matches: [{ ...legacyMatch(), reveal_cursor } as DocFindMatch] }));
  }
  const current = legacyPage();
  assert.equal(validateDocumentFindPage(current, legacySource).matches[0].reveal_cursor, '');
  invalid(page({ matches: [legacyMatch()] }));
  const root = page({ matches: [match({ reveal_cursor: 'z'.repeat(2731) })] });
  assert.equal(validateDocumentFindPage(root, source), root);
});

test('legacy continuation cursors have a 2731-character base64url bound independent of root find tokens', () => {
  for (const cursor of ['', 'a'.repeat(2732), 'a'.repeat(5462), 'bad=', 'bad+', 'bad/', 'bad token', 'bad\n', '🙂', null, 5, {}]) {
    assert.throws(() => documentFindParameters('needle', legacySource, { source: 0, cursor } as DocumentFindPosition), DocumentProjectionError);
    invalidLegacy({ ...legacyPage(), next_cursor: cursor });
  }
  for (const next_cursor of [undefined, false, []]) invalidLegacy({ ...legacyPage(), next_cursor });
  const cursor = 'a'.repeat(2731);
  assert.deepEqual([...documentFindParameters('needle', legacySource, { source: 0, cursor })], [
    ['q', 'needle'], ['legacy', '1'], ['cursor', cursor],
  ]);
  const current = legacyPage({ next_cursor: cursor });
  assert.equal(validateDocumentFindPage(current, legacySource), current);
  const root = page({ next_cursor: 'a'.repeat(5462) });
  assert.equal(validateDocumentFindPage(root, source), root);
  for (const position of [{ source: -1 }, { source: 0.5 }, { source: NaN }, { source: Number.MAX_SAFE_INTEGER + 1 },
    { source: 0, frontier: { index: 1, field: 0, byte: 0 } }, { source: 0, frontier: { index: 30, field: 1, byte: 0 } }]) {
    assert.throws(() => documentFindParameters('needle', legacySource, position), DocumentProjectionError);
  }
});

test('legacy pages and matches retain exact own-key shapes and reject duplicates, sparse lists and range mismatches', () => {
  for (const value of [undefined, null, [], {}, 'find-legacy-v1', 5, { ...legacyPage(), hash: 'other' },
    { ...legacyPage(), from: 1 }, { ...legacyPage(), to: 31 }, { ...legacyPage(), matches: null }, { ...legacyPage(), matches: {} },
    { ...legacyPage(), matches: [null] }, { ...legacyPage(), matches: Array(1) }, { ...legacyPage(), unexpected: true },
    { ...legacyPage(), rules: 1 }, { ...legacyPage(), events: [] }, { ...legacyPage(), matches: [legacyMatch(), legacyMatch()] },
    { hash: 'document', envelope: {}, events: [], total: 40, offset: 2, next: 3, inherited: 2 }]) invalidLegacy(value);
  for (const key of Object.keys(legacyPage())) {
    const missing: Record<string, unknown> = { ...legacyPage() };
    delete missing[key];
    invalidLegacy(missing);
    invalidLegacy(Object.assign(Object.create({ [key]: legacyPage()[key as keyof DocFindPage] }), missing));
  }
  for (const key of Object.keys(legacyMatch())) {
    const missing: Record<string, unknown> = { ...legacyMatch() };
    delete missing[key];
    invalidLegacy(legacyPage({ matches: [missing as unknown as DocFindMatch] }));
    const inherited = Object.assign(Object.create({ [key]: legacyMatch()[key as keyof DocFindMatch] }), missing);
    invalidLegacy(legacyPage({ matches: [inherited] }));
  }
  invalidLegacy(legacyPage({ matches: [{ ...legacyMatch(), unknown: 'payload' } as DocFindMatch] }));
});

test('legacy frontiers retain strict lexicographic progress, terminal coordinates and bounded integer checks', () => {
  const previous = { index: 4, field: 3, byte: 12 };
  for (const change of [{ event_index: 4, field: 3, byte_offset: 13 }, { event_index: 4, field: 4, byte_offset: 0 },
    { event_index: 5, field: 0, byte_offset: 0 }, { event_index: legacySource.to, field: 0, byte_offset: 0, next_cursor: null }]) {
    const current = legacyPage({ matches: [], ...change });
    assert.equal(validateDocumentFindPage(current, legacySource, previous), current);
  }
  for (const change of [{ event_index: 4, field: 3, byte_offset: 12 }, { event_index: 4, field: 3, byte_offset: 11 },
    { event_index: 4, field: 2, byte_offset: 100 }, { event_index: 3, field: 4, byte_offset: 100 },
    { event_index: legacySource.to, field: 1, next_cursor: null }, { event_index: legacySource.to, byte_offset: 1, next_cursor: null },
    { event_index: legacySource.to }, { event_index: 5, next_cursor: null }]) invalidLegacy(legacyPage({ matches: [], ...change }), previous);
  invalidLegacy(legacyPage({ event_index: legacySource.from, matches: [] }));
  for (const prior of [{ index: 1, field: 0, byte: 0 }, { index: 31, field: 0, byte: 0 }, { index: 3, field: -1, byte: 0 },
    { index: 3, field: 0, byte: 0.5 }, { index: legacySource.to, field: 0, byte: 1 }, { index: 3, field: findFields + 1, byte: 0 },
    { index: 3, field: 0, byte: documentBytes + 1 }]) invalidLegacy(legacyPage({ matches: [] }), prior);
  for (const value of [-1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '3', null, undefined]) {
    for (const key of ['event_index', 'field', 'byte_offset']) invalidLegacy({ ...legacyPage(), [key]: value });
    for (const key of ['event_index', 'field', 'byte_offset', 'byte_length', 'preview_byte_offset']) {
      invalidLegacy(legacyPage({ matches: [{ ...legacyMatch(), [key]: value }] }));
    }
  }
  for (const change of [{ event_index: legacySource.from - 1 }, { event_index: legacySource.to + 1 }, { field: findFields + 1 },
    { byte_offset: documentBytes + 1 }]) invalidLegacy(legacyPage(change));
  for (const change of [{ event_index: legacySource.from - 1 }, { event_index: legacySource.to }, { field: findFields + 1 },
    { byte_length: 0 }, { byte_length: 1025 }, { byte_offset: documentBytes, preview_byte_offset: documentBytes },
    { preview_byte_offset: 5 }]) invalidLegacy(legacyPage({ matches: [legacyMatch(change)] }));
  for (const raw of [null, undefined, 0, 1, 'false', {}, []]) invalidLegacy(legacyPage({ matches: [{ ...legacyMatch(), raw } as DocFindMatch] }));
  const maximum = legacyPage({ field: findFields, byte_offset: documentBytes, matches: [] });
  assert.equal(validateDocumentFindPage(maximum, legacySource), maximum);
});

test('legacy matches retain the shared cap, ordering and prior-inclusive frontier-exclusive range', () => {
  const matches = Array.from({ length: 20 }, (_, index) => legacyMatch({ byte_offset: index, byte_length: 1, preview: 'a', preview_byte_offset: index }));
  const full = legacyPage({ matches, event_index: legacySource.from, byte_offset: 20 });
  assert.equal(validateDocumentFindPage(full, legacySource), full);
  invalidLegacy(legacyPage({ matches: [...matches, legacyMatch({ event_index: 3 })], event_index: 4 }));
  invalidLegacy(legacyPage({ matches: [...matches].reverse() }));
  invalidLegacy(legacyPage({ matches: [legacyMatch({ event_index: 3 }), legacyMatch()], event_index: 4 }));
  invalidLegacy(legacyPage({ matches: [legacyMatch({ field: 1 }), legacyMatch()] }));
  invalidLegacy(legacyPage({ matches: [legacyMatch({ event_index: 3 })] }));
  invalidLegacy(legacyPage({ matches: [legacyMatch({ event_index: 4 })] }));
  invalidLegacy(legacyPage({ event_index: 2, byte_offset: 4 }));
  const prior = { index: 2, field: 0, byte: 4 };
  const current = legacyPage({ event_index: 2, byte_offset: 5 });
  assert.equal(validateDocumentFindPage(current, legacySource, prior), current);
  invalidLegacy(current, { ...prior, byte: 5 });
  const overlapping = legacyPage({ event_index: 2, byte_offset: 4, matches: [
    legacyMatch({ byte_offset: 1, byte_length: 3, preview: 'banana' }), legacyMatch({ byte_offset: 3, byte_length: 3, preview: 'banana' }),
  ] });
  assert.equal(validateDocumentFindPage(overlapping, legacySource), overlapping);
});

test('legacy previews retain shared UTF-8 boundaries, document byte limits and rune context budgets', () => {
  const preview = 'aé\ud55c🙂z';
  for (const [byte_offset, byte_length] of [[0, 1], [1, 2], [3, 3], [6, 4], [10, 1], [1, 9]]) {
    const current = legacyPage({ matches: [legacyMatch({ preview, byte_offset, byte_length })] });
    assert.equal(validateDocumentFindPage(current, legacySource), current);
  }
  for (const [byte_offset, byte_length] of [[2, 1], [4, 2], [7, 3], [1, 1], [3, 1], [6, 3], [11, 1], [0, 12]]) {
    invalidLegacy(legacyPage({ matches: [legacyMatch({ preview, byte_offset, byte_length })] }));
  }
  for (const previewValue of ['', null, undefined, 7, [], '\ud800', '\udfff', 'a\ud800b', 'a\udfff🙂']) {
    invalidLegacy(legacyPage({ matches: [legacyMatch({ preview: previewValue as string, byte_offset: 0, byte_length: 1 })] }));
  }
  const shifted = legacyPage({ matches: [legacyMatch({ preview, preview_byte_offset: 123, byte_offset: 129, byte_length: 4, raw: true })] });
  assert.equal(validateDocumentFindPage(shifted, legacySource), shifted);
  invalidLegacy(legacyPage({ matches: [legacyMatch({ preview: 'aa', preview_byte_offset: documentBytes - 1,
    byte_offset: documentBytes - 1, byte_length: 1 })] }));
  const maximum = legacyPage({ matches: [legacyMatch({ preview: '🙂'.repeat(32) + '\ud55c'.repeat(256) + 'é'.repeat(32), byte_offset: 128, byte_length: 768 })] });
  assert.equal(validateDocumentFindPage(maximum, legacySource), maximum);
  const astral = legacyPage({ matches: [legacyMatch({ preview: '🙂'.repeat(320), byte_offset: 128, byte_length: 1024 })] });
  assert.equal(validateDocumentFindPage(astral, legacySource), astral);
  for (const change of [
    { preview: '🙂'.repeat(321), byte_offset: 128, byte_length: 1024 },
    { preview: 'a'.repeat(257), byte_offset: 0, byte_length: 257 },
    { preview: 'a'.repeat(33) + 'x', byte_offset: 33, byte_length: 1 },
    { preview: 'x' + 'a'.repeat(33), byte_offset: 0, byte_length: 1 },
    { preview: 'a'.repeat(100_000), byte_offset: 0, byte_length: 1 },
  ]) invalidLegacy(legacyPage({ matches: [legacyMatch(change)] }));
});

const displayBytes = 512 * 1024;
const displaySource: RootDocumentFindSource = { ...source, representation: 'display-v1' };
const legacyDisplaySource: LegacyDocumentFindSource = { ...legacySource, representation: 'display-v1' };

function displayMatch(change: Partial<DocFindMatch> = {}): DocFindMatch {
  return match({ source_range: { from: 120_000, to: 120_006 }, ...change });
}

function legacyDisplayMatch(change: Partial<DocFindMatch> = {}): DocFindMatch {
  return displayMatch({ reveal_cursor: '', ...change });
}

function displayPage(change: Partial<DocFindPage> = {}): DocFindPage {
  return page({ projection: 'find-display-v1', matches: [displayMatch()], ...change });
}

function legacyDisplayPage(change: Partial<DocFindPage> = {}): DocFindPage {
  return legacyPage({ projection: 'find-legacy-display-v1', matches: [legacyDisplayMatch()], ...change });
}

const displayFixtures = [
  { source, displaySource, sourcePage: page, page: displayPage, match: displayMatch },
  { source: legacySource, displaySource: legacyDisplaySource, sourcePage: legacyPage, page: legacyDisplayPage, match: legacyDisplayMatch },
];

test('display parameters append the representation to initial requests and every opaque continuation without changing source v1', () => {
  const query = ' "Needle" + & ?\n\ud55c🙂 ';
  const positions: (DocumentFindPosition | undefined)[] = [undefined, { source: 0 },
    { source: 7, cursor: 'not_decoded-or-reencoded', frontier: { index: 5, field: 0, byte: 30 } },
    { source: 7, cursor: 'second_opaque-token', frontier: { index: 5, field: 0, byte: displayBytes } }];
  for (const fixture of displayFixtures) {
    const before = structuredClone(fixture.displaySource);
    for (const position of positions) {
      const originalPosition = structuredClone(position);
      const parameters = documentFindParameters(query, fixture.displaySource, position);
      assert.deepEqual([...parameters], [...documentFindParameters(query, fixture.source, position), ['representation', 'display-v1']]);
      assert.deepEqual(parameters.getAll('representation'), ['display-v1']);
      assert.deepEqual([...new URLSearchParams(parameters.toString())], [...parameters]);
      assert.deepEqual(position, originalPosition);
    }
    assert.deepEqual(fixture.displaySource, before);
    assert.equal(documentFindParameters(query, { ...fixture.source, representation: undefined }).has('representation'), false);
    const noBase = structuredClone(fixture.displaySource);
    noBase.from = 0;
    noBase.selection = { ...noBase.selection, base: undefined };
    noBase.context = { ...noBase.context, base: '', start: 0, inherited: 0 };
    const parameters = documentFindParameters(query, noBase);
    assert.equal(parameters.has('base'), false);
    assert.equal(parameters.get('representation'), 'display-v1');
  }
  assert.deepEqual([...documentFindParameters(query, { ...displaySource, protocol: 'visible-v1' })],
    [...documentFindParameters(query, displaySource)]);
});

test('both find protocols reject explicit empty and unknown representation values before generating requests or accepting pages', () => {
  for (const fixture of displayFixtures) {
    for (const representation of ['', 'source-v1', 'display-v2', 'DISPLAY-V1', ' display-v1', 'display-v1 ', null, 0, false, [], {}]) {
      const changed = { ...fixture.source, representation } as unknown as DocumentFindSource;
      assert.throws(() => documentFindParameters('needle', changed), DocumentProjectionError);
      assert.throws(() => documentFindParameters('needle', changed, { source: 0, cursor: 'opaque' }), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage(fixture.page(), changed), DocumentProjectionError);
      assert.throws(() => documentFindFragmentParameters(changed, fixture.match(), 0, 64 * 1024), DocumentProjectionError);
    }
  }
});

test('root and legacy find pages bind the exact source or display projection without accepting mixed modes or extra response fields', () => {
  const modes = displayFixtures.flatMap(fixture => [
    { source: fixture.source, page: fixture.sourcePage() }, { source: fixture.displaySource, page: fixture.page() },
  ]);
  for (const [requestedIndex, requested] of modes.entries()) {
    for (const [responseIndex, response] of modes.entries()) {
      if (requestedIndex === responseIndex) assert.equal(validateDocumentFindPage(response.page, requested.source), response.page);
      else assert.throws(() => validateDocumentFindPage(response.page, requested.source), DocumentProjectionError);
    }
  }
  for (const fixture of displayFixtures) {
    for (const change of [{ projection: undefined }, { projection: null }, { projection: 'find-display-v2' }, { projection: 'visible-v1' },
      { generation: 'other' }, { hash: 'other' }, { from: 1 }, { to: 31 }, { representation: 'display-v1' }, { unknown: true }]) {
      assert.throws(() => validateDocumentFindPage({ ...fixture.page(), ...change }, fixture.displaySource), DocumentProjectionError);
    }
    const terminal = fixture.page({ event_index: fixture.source.to, matches: [], next_cursor: null });
    assert.equal(validateDocumentFindPage(terminal, fixture.displaySource), terminal);
    const empty = { ...fixture.displaySource, from: fixture.source.to };
    const emptyPage = { ...terminal, from: empty.from };
    assert.equal(validateDocumentFindPage(emptyPage, empty), emptyPage);
  }
});

test('display matches require field zero, raw false and an own bounded source range while retaining opaque reveal selectors', () => {
  for (const fixture of displayFixtures) {
    const selected = Object.freeze(fixture.match({ source_range: Object.freeze({ from: documentBytes - 1, to: documentBytes }) }));
    const current = Object.freeze(fixture.page({ matches: Object.freeze([selected]) as unknown as DocFindMatch[] }));
    assert.equal(validateDocumentFindPage(current, fixture.displaySource), current);
    for (const change of [{ field: 1 }, { field: findFields }, { field: -1 }, { field: 0.5 }, { field: '0' },
      { raw: true }, { raw: undefined }, { raw: null }, { raw: 0 }, { raw: 'false' },
      { raw: true, source_range: { from: 4, to: 10 } }, { source_range: undefined }, { source_range: null },
      { source_range: {} }, { source_range: { from: -1, to: 1 } }, { source_range: { from: 4, to: 4 } },
      { source_range: { from: 5, to: 4 } }, { source_range: { from: 0, to: documentBytes + 1 } },
      { source_range: { from: 1.5, to: 4 } }, { source_range: { from: '1', to: 4 } }, { source_range: { from: 1, to: 4, extra: true } }]) {
      const changed = { ...fixture.match(), ...change };
      assert.throws(() => validateDocumentFindPage(fixture.page({ matches: [changed as DocFindMatch] }), fixture.displaySource), DocumentProjectionError);
    }
    const missing: Record<string, unknown> = { ...fixture.match() };
    delete missing.source_range;
    for (const selected of [missing, Object.assign(Object.create({ source_range: { from: 4, to: 10 } }), missing)]) {
      assert.throws(() => validateDocumentFindPage(fixture.page({ matches: [selected as DocFindMatch] }), fixture.displaySource), DocumentProjectionError);
    }
    const wrongReveal = fixture.match({ reveal_cursor: fixture.source.protocol === 'legacy' ? 'opaque' : '' });
    assert.throws(() => validateDocumentFindPage(fixture.page({ matches: [wrongReveal] }), fixture.displaySource), DocumentProjectionError);
    const sourceOnly = fixture.sourcePage({ matches: [fixture.match({ field: 1, raw: true, source_range: undefined })] });
    delete sourceOnly.matches[0].source_range;
    assert.equal(validateDocumentFindPage(sourceOnly, fixture.source), sourceOnly);
  }
});

test('display match and preview coordinates stay within 512 KiB while source ranges retain the document byte bound', () => {
  for (const fixture of displayFixtures) {
    const nearEnd = fixture.match({ byte_offset: displayBytes - 4, byte_length: 4, preview: '🙂', preview_byte_offset: displayBytes - 4,
      source_range: { from: documentBytes - 4, to: documentBytes } });
    const maximum = fixture.page({ matches: [nearEnd] });
    assert.equal(validateDocumentFindPage(maximum, fixture.displaySource), maximum);
    for (const change of [
      { byte_offset: displayBytes, byte_length: 1, preview: 'x', preview_byte_offset: displayBytes },
      { byte_offset: displayBytes + 1, byte_length: 1, preview: 'x', preview_byte_offset: displayBytes + 1 },
      { byte_offset: displayBytes - 1, byte_length: 2, preview: 'xx', preview_byte_offset: displayBytes - 1 },
      { byte_offset: displayBytes - 1, byte_length: 1, preview: 'xx', preview_byte_offset: displayBytes - 1 },
      { byte_offset: displayBytes - 3, byte_length: 3, preview: '🙂', preview_byte_offset: displayBytes - 4 },
    ]) {
      const selected = fixture.match(change);
      assert.throws(() => validateDocumentFindPage(fixture.page({ matches: [selected] }), fixture.displaySource), DocumentProjectionError);
    }
    const sourceCoordinates = fixture.sourcePage({ matches: [fixture.match({ byte_offset: documentBytes - 1, byte_length: 1,
      preview: 'x', preview_byte_offset: documentBytes - 1 })] });
    assert.equal(validateDocumentFindPage(sourceCoordinates, fixture.source), sourceCoordinates);
    assert.throws(() => validateDocumentFindPage({ ...sourceCoordinates, projection: fixture.page().projection }, fixture.displaySource), DocumentProjectionError);
  }
});

test('display request, current and previous frontiers require field zero and a byte bound of 512 KiB with strict progress', () => {
  for (const fixture of displayFixtures) {
    const maximum = fixture.page({ event_index: fixture.source.from, field: 0, byte_offset: displayBytes, matches: [] });
    assert.equal(validateDocumentFindPage(maximum, fixture.displaySource), maximum);
    const previous = { index: fixture.source.from, field: 0, byte: displayBytes };
    const progressed = fixture.page({ matches: [] });
    assert.equal(validateDocumentFindPage(progressed, fixture.displaySource, previous), progressed);
    for (const cursor of [undefined, 'opaque']) {
      assert.equal(documentFindParameters('needle', fixture.displaySource, { source: 0, cursor, frontier: previous }).get('representation'), 'display-v1');
    }
    for (const frontier of [{ ...previous, field: 1 }, { ...previous, field: findFields }, { ...previous, byte: displayBytes + 1 },
      { ...previous, byte: -1 }, { ...previous, byte: 0.5 }, { ...previous, byte: NaN },
      { index: fixture.source.to, field: 0, byte: 1 }]) {
      for (const cursor of [undefined, 'opaque']) {
        assert.throws(() => documentFindParameters('needle', fixture.displaySource, { source: 0, cursor, frontier }), DocumentProjectionError);
      }
      assert.throws(() => validateDocumentFindPage(progressed, fixture.displaySource, frontier), DocumentProjectionError);
      const changed = fixture.page({ event_index: frontier.index, field: frontier.field, byte_offset: frontier.byte, matches: [] });
      assert.throws(() => validateDocumentFindPage(changed, fixture.displaySource), DocumentProjectionError);
    }
    for (const change of [{ event_index: fixture.source.from, byte_offset: displayBytes },
      { event_index: fixture.source.from, byte_offset: displayBytes - 1 }, { event_index: fixture.source.to, byte_offset: 1, next_cursor: null }]) {
      assert.throws(() => validateDocumentFindPage(fixture.page({ ...change, matches: [] }), fixture.displaySource, previous), DocumentProjectionError);
    }
    const sourceFrontier = fixture.sourcePage({ field: findFields, byte_offset: documentBytes, matches: [] });
    assert.equal(validateDocumentFindPage(sourceFrontier, fixture.source), sourceFrontier);
  }
});

test('display pages retain match ordering, caps and protocol-specific opaque cursor bounds', () => {
  for (const fixture of displayFixtures) {
    const matches = Array.from({ length: 20 }, (_, index) => fixture.match({ byte_offset: index, byte_length: 1, preview: 'a', preview_byte_offset: index }));
    const full = fixture.page({ matches, event_index: fixture.source.from, byte_offset: 20 });
    assert.equal(validateDocumentFindPage(full, fixture.displaySource), full);
    for (const changed of [fixture.page({ matches: [...matches, fixture.match({ event_index: 3 })], event_index: 4 }),
      fixture.page({ matches: [...matches].reverse() }), fixture.page({ matches: [fixture.match(), fixture.match()] }),
      fixture.page({ event_index: fixture.source.from, byte_offset: 4 })]) {
      assert.throws(() => validateDocumentFindPage(changed, fixture.displaySource), DocumentProjectionError);
    }
    const maximum = fixture.source.protocol === 'legacy' ? 2731 : 5462;
    const cursor = 'a'.repeat(maximum);
    assert.equal(documentFindParameters('needle', fixture.displaySource, { source: 0, cursor }).get('cursor'), cursor);
    const current = fixture.page({ next_cursor: cursor });
    assert.equal(validateDocumentFindPage(current, fixture.displaySource), current);
    for (const cursor of ['a'.repeat(maximum + 1), 'bad=', 'bad+', 'bad/', '', 'bad\n']) {
      assert.throws(() => documentFindParameters('needle', fixture.displaySource, { source: 0, cursor }), DocumentProjectionError);
      assert.throws(() => validateDocumentFindPage({ ...current, next_cursor: cursor }, fixture.displaySource), DocumentProjectionError);
    }
  }
});

test('display fragment requests and responses retain the exact source fragment protocol with no representation parameter', () => {
  const offset = displayBytes * 4;
  const budget = 64 * 1024;
  for (const fixture of displayFixtures) {
    const selected = fixture.match({ source_range: { from: offset, to: offset + 6 } });
    const parameters = documentFindFragmentParameters(fixture.displaySource, selected, offset, budget);
    assert.deepEqual([...parameters], [...documentFindFragmentParameters(fixture.source, selected, offset, budget)]);
    assert.equal(parameters.has('representation'), false);
    assert.deepEqual(Object.fromEntries(parameters), fixture.source.protocol === 'legacy'
      ? { legacy: '1', index: '2', total: '40', offset: String(offset), budget: String(budget) }
      : { selection: selected.reveal_cursor, offset: String(offset), budget: String(budget) });
    const fragment = { projection: fixture.source.protocol === 'legacy' ? 'find-legacy-fragment-v1' : 'find-fragment-v1',
      hash: 'document', generation: fixture.source.protocol === 'legacy' ? '' : 'generation', event_index: selected.event_index,
      byte_offset: offset, total_bytes: offset + 10, json_fragment: 'NeedLE🙂', next: -1, previous: offset - budget };
    assert.equal(validateDocumentFindFragment(fragment, fixture.displaySource, selected, offset, budget), fragment);
    for (const change of [{ projection: 'find-display-fragment-v1' }, { projection: 'find-legacy-display-fragment-v1' }, { representation: 'display-v1' }]) {
      assert.throws(() => validateDocumentFindFragment({ ...fragment, ...change }, fixture.displaySource, selected, offset, budget), DocumentProjectionError);
    }
  }
});
