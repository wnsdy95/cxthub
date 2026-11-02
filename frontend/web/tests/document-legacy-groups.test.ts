import assert from 'node:assert/strict';
import { test } from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { documentEventIsPrompt, documentEventIsRendered, documentEventIsSummary } from '../src/documentEventClassification';
import { legacyDocumentTurns, legacyResponseGroups, legacyRowSegments, legacyGroupSegments } from '../src/documentLegacyGroups';
import { recordLegacyDocumentPage } from '../src/documentLegacyLedger';
import { documentVisibleKey, validateLegacyDocument } from '../src/documentVisible';
import type { DocViewEvent } from '../src/types';
import { visibleMessage, visibleSelection } from './documentVisibleFixture';

test('legacy turns and response work span page boundaries without retaining source bodies', () => {
  const client = new QueryClient();
  const events: DocViewEvent[] = [
    { kind: 'reasoning', redacted_summary: 'Leading reasoning' },
    { ...visibleMessage(1, '[cxt] This session was resumed from a branch context seed. old memory', 'user'), seed_summary: true },
    visibleMessage(2, 'First actual prompt', 'user'),
    { kind: 'tool_call', tool_name: 'shell', input: { secret: 'tool input' } },
    { kind: 'reasoning', redacted_summary: 'Work before response' },
    { kind: 'reasoning', locked: { provider: 'codex', scheme: 'encrypted', blob: 'ciphertext' } },
    visibleMessage(6, 'First response'),
    { kind: 'tool_result', output: { secret: 'tool output' } },
    visibleMessage(8, 'Second response'),
    visibleMessage(9, 'Next prompt', 'user'),
    { kind: 'tool_call', tool_name: 'unfinished' },
  ];
  try {
    const pages = [0, 4, 8].map((offset, position) => recordLegacyDocumentPage(client, documentVisibleKey(visibleSelection),
      validateLegacyDocument({ hash: visibleSelection.hash, envelope: {}, total: events.length, offset, inherited: position ? 0 : 2,
        next: position === 2 ? -1 : offset + 4, events: events.slice(offset, offset + 4) }, visibleSelection, offset), visibleSelection,
      position ? { kind: 'legacy', next: offset, total: events.length } : undefined));
    const turns = legacyDocumentTurns(pages);
    assert.deepEqual(turns.map(turn => [turn.index, turn.head?.index ?? null, turn.body.map(row => row.index)]), [
      [-1, null, [0, 1]], [2, 2, [3, 4, 6, 7, 8]], [9, 9, [10]],
    ]);
    assert.deepEqual(legacyResponseGroups(turns[1].body).map(group => [group.pre.map(row => row.index), group.message?.index ?? null]), [
      [[3, 4], 6], [[7], 8],
    ]);
    assert.deepEqual(legacyResponseGroups(turns[2].body).map(group => [group.pre.map(row => row.index), group.message?.index ?? null]), [[[10], null]]);
    assert.deepEqual(legacyRowSegments(turns[1].body).map(segment => [segment.page.offset, segment.rows.map(row => row.index)]), [
      [0, [3]], [4, [4, 6, 7]], [8, [8]],
    ]);
    const encoded = JSON.stringify(turns);
    for (const text of ['First actual prompt', 'tool input', 'Leading reasoning', 'tool output', 'ciphertext']) assert.equal(encoded.includes(text), false);
    assert.equal(encoded.includes('"events":'), false);
  } finally { client.clear(); }
});

test('legacy render segments cap each mounted row group while preserving absolute positions', () => {
  const client = new QueryClient();
  try {
    const page = recordLegacyDocumentPage(client, documentVisibleKey(visibleSelection), validateLegacyDocument({
      hash: visibleSelection.hash, envelope: {}, offset: 142, total: 654, inherited: 0, next: -1,
      events: Array.from({ length: 512 }, () => ({ kind: 'tool_call', tool_name: 'work' })),
    }, visibleSelection, 142), { ...visibleSelection, start: 142 });
    const turns = legacyDocumentTurns([page]);
    assert.equal(turns.length, 1);
    assert.equal(turns[0].index, 141);
    const segments = legacyRowSegments(turns[0].body);
    assert.equal(segments.length, 8);
    assert.deepEqual(segments.flatMap(segment => segment.rows.map(row => row.index)), Array.from({ length: 512 }, (_, index) => 142 + index));
    assert.equal(segments.every(segment => segment.rows.length === 64), true);
  } finally { client.clear(); }
});

test('legacy classification preserves explicit seed overrides and empty reasoning behavior', () => {
  const seed = visibleMessage(0, '[cxt seed] Branch-switch context: literal prompt', 'user');
  assert.equal(documentEventIsSummary(seed), false);
  assert.equal(documentEventIsPrompt(seed), true);
  const { seed_summary: _seedFlag, ...oldSeed } = seed;
  assert.equal(documentEventIsSummary(oldSeed), true);
  assert.equal(documentEventIsPrompt(oldSeed), false);
  assert.equal(documentEventIsPrompt({ ...seed, compact_summary: true }), false);
  assert.equal(documentEventIsRendered({ kind: 'reasoning' }), false);
  assert.equal(documentEventIsRendered({ kind: 'reasoning', redacted_summary: '   ' }), false);
  assert.equal(documentEventIsRendered({ kind: 'reasoning', deferred: true }), true);
  assert.equal(documentEventIsRendered({ kind: 'reasoning', redacted_summary: 'visible', empty_reasoning: true }), false);
});

test('legacy turn windows retain stable twenty-turn boundaries as new pages arrive', () => {
  const client = new QueryClient();
  try {
    const page = recordLegacyDocumentPage(client, documentVisibleKey(visibleSelection), validateLegacyDocument({
      hash: visibleSelection.hash, envelope: {}, offset: 0, total: 50, inherited: 0, next: -1,
      events: Array.from({ length: 50 }, (_, index) => visibleMessage(index, `Prompt ${index}`, 'user')),
    }, visibleSelection, 0), visibleSelection);
    const turns = legacyDocumentTurns([page]);
    assert.deepEqual(legacyGroupSegments(turns).map(segment => segment.map(turn => turn.index)), [
      Array.from({ length: 20 }, (_, index) => index), Array.from({ length: 20 }, (_, index) => index + 20),
      Array.from({ length: 10 }, (_, index) => index + 40),
    ]);
    assert.deepEqual(legacyGroupSegments(turns.slice(0, 23))[0], legacyGroupSegments(turns)[0]);
  } finally { client.clear(); }
});
