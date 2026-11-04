import assert from 'node:assert/strict';
import { test } from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../src/api';
import { type DocumentFindSource } from '../src/documentFind';
import { DocumentProjectionError } from '../src/documentVisible';
import { readDocumentFindBatch, documentFindSourceIdentity, DocumentFindReadError } from '../src/useDocumentFind';

function source(id: string): DocumentFindSource {
  return { id, selection: { repoId: 'repo', hash: id, start: 0, mode: 'all' },
    context: { hash: id, base: '', generation: 'generation', rules: 1, view: 'all', start: 0, total: 2, inherited: 0 },
    from: 0, to: 2, replay: `self_${id}` };
}

function page(hash: string, change: Record<string, unknown> = {}) {
  return { projection: 'find-v1', hash, generation: 'generation', from: 0, to: 2, event_index: 2,
    field: 0, byte_offset: 0, matches: [], next_cursor: null, ...change };
}

test('find scan continues empty cursors and composed sources without retaining old payloads', async context => {
  const client = new QueryClient();
  const calls: [string, string | null][] = [];
  context.mock.method(api, 'getDocFindPage', async (_repo: string, hash: string, params: URLSearchParams) => {
    calls.push([hash, params.get('cursor')]);
    return hash === 'first' ? params.has('cursor') ? page(hash) : page(hash, { event_index: 1, next_cursor: 'continue_first' })
      : page(hash, { matches: [{ event_index: 0, field: 0, byte_offset: 0, byte_length: 6, raw: false,
        preview: 'needle', preview_byte_offset: 0, reveal_cursor: 'source_reveal' }] });
  });
  try {
    const result = await readDocumentFindBatch(client, { id: 'run', query: 'needle', sources: [source('first'), source('second')] },
      { source: 0 }, new AbortController().signal);
    assert.deepEqual(calls, [['first', null], ['first', 'continue_first'], ['second', null]]);
    assert.equal(result.matches.length, 1);
    assert.equal(result.matches[0].source.id, 'second');
    assert.equal(result.next, undefined);
    await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(client.getQueryCache().getAll().filter(query => query.state.data).length, 0);
  } finally { client.clear(); }
});

test('denied and stale find reads identify their actual source without fallback', async context => {
  const client = new QueryClient();
  let calls = 0;
  context.mock.method(api, 'getDocFindPage', async (_repo: string, hash: string) => {
    calls++;
    if (hash === 'first') return page(hash);
    throw new ApiError('revoked', 'forbidden', 403);
  });
  try {
    await assert.rejects(readDocumentFindBatch(client, { id: 'denied', query: 'needle', sources: [source('first'), source('second')] },
      { source: 0 }, new AbortController().signal), error => error instanceof DocumentFindReadError &&
      error.sourceId === 'second' && error.cause instanceof ApiError && error.cause.code === 'forbidden');
    assert.equal(calls, 2);
    context.mock.method(api, 'getDocFindPage', async () => page('first', { generation: 'changed' }));
    await assert.rejects(readDocumentFindBatch(client, { id: 'stale', query: 'needle', sources: [source('first')] },
      { source: 0 }, new AbortController().signal), error => error instanceof DocumentFindReadError && error.cause instanceof DocumentProjectionError);
  } finally { client.clear(); }
});

test('closing a find action aborts the active request rather than advancing to another source', async context => {
  const client = new QueryClient();
  const controller = new AbortController();
  let calls = 0;
  let started: () => void = () => undefined;
  const ready = new Promise<void>(resolve => { started = resolve; });
  context.mock.method(api, 'getDocFindPage', (_repo: string, _hash: string, _params: URLSearchParams, signal?: AbortSignal) => {
    calls++;
    started();
    return new Promise((_resolve, reject) => signal!.addEventListener('abort', () => reject(signal!.reason), { once: true }));
  });
  try {
    const pending = readDocumentFindBatch(client, { id: 'cancel', query: 'needle', sources: [source('first'), source('second')] },
      { source: 0 }, controller.signal);
    await ready;
    controller.abort();
    await assert.rejects(pending, error => error instanceof DOMException && error.name === 'AbortError');
    assert.equal(calls, 1);
  } finally { client.clear(); }
});

test('mixed find reads retain their protocol and immutable hash without synthesizing a legacy generation', async context => {
  const client = new QueryClient();
  const calls: [string, string | null, string | null][] = [];
  const legacy: DocumentFindSource = { id: 'legacy', protocol: 'legacy',
    selection: { repoId: 'repo', hash: 'legacy', start: 0, mode: 'all' },
    context: { hash: 'legacy', base: '', view: 'all', start: 0, total: 2, inherited: 0 }, from: 0, to: 2 };
  const initial = documentFindSourceIdentity(legacy);
  context.mock.method(api, 'getDocFindPage', async (_repo: string, hash: string, params: URLSearchParams) => {
    calls.push([hash, params.get('legacy'), params.get('selection')]);
    return hash === 'first' ? page(hash) : page(hash, { projection: 'find-legacy-v1', generation: '',
      matches: [{ event_index: 0, field: 0, byte_offset: 0, byte_length: 6, raw: false,
        preview: 'needle', preview_byte_offset: 0, reveal_cursor: '' }] });
  });
  try {
    const result = await readDocumentFindBatch(client, { id: 'mixed', query: 'needle', sources: [source('first'), legacy] },
      { source: 0 }, new AbortController().signal);
    assert.deepEqual(calls, [['first', null, 'self_first'], ['legacy', '1', null]]);
    assert.equal(result.matches[0].source, legacy);
    assert.equal(result.matches[0].match.reveal_cursor, '');
    assert.equal(documentFindSourceIdentity(legacy), initial);
    assert.equal('generation' in legacy.context, false);
    assert.equal('replay' in legacy, false);
  } finally { client.clear(); }
});
