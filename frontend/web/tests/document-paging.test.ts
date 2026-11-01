import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, type DocViewEventPage } from '../src/api';
import { validateDocViewPage, validateDocEventDetail, validateDocFragment } from '../src/documentPaging';
import { documentVisibleKey } from '../src/documentVisible';
import type { DocViewMode } from '../src/types';

const hash = 'sha256:document';
const page: DocViewEventPage = {
  hash, envelope: { cir_version: '2', source_provider: 'codex' },
  offset: 99, total: 102, next: 101, inherited: 100,
  events: [
    { kind: 'message', role: 'user', seq: 400, blocks: [{ type: 'text', text: 'prompt' }] },
    { kind: 'tool_call', seq: 900, tool_name: 'Bash', deferred: true, event_bytes: 512 * 1024 * 1024 },
  ],
};

test('view page ranges preserve absolute indices, hidden placeholders and inherited boundaries', () => {
  const result = validateDocViewPage(page, hash, -1);
  assert.deepEqual(result.events.map(event => event.event_index), [99, 100]);
  assert.equal(result.events[1].deferred, true);
  assert.equal(result.inherited, 100);
  assert.equal(page.events[0].event_index, undefined);
  for (const invalid of [
    { ...page, next: 99 }, { ...page, next: 100 }, { ...page, next: -1 },
    { ...page, next: 102 }, { ...page, inherited: 103 }, { ...page, total: 100 },
    { ...page, events: [{ ...page.events[0], event_index: 400 }, page.events[1]] },
    { ...page, events: Array.from({ length: 513 }, () => page.events[1]), total: 612, next: -1 },
  ]) assert.throws(() => validateDocViewPage(invalid, hash, -1));
  assert.throws(() => validateDocViewPage(page, hash, 0));
  assert.throws(() => validateDocViewPage(page, 'another', -1));
  assert.equal(validateDocViewPage({ ...page, events: [], offset: 102, next: -1 }, hash, 102).next, -1);
  const selection = { repoId: 'repo', hash, base: 'base', start: -1, mode: 'chat' as DocViewMode };
  const keys = ['all', 'chat', 'prompts'].map(mode => JSON.stringify(documentVisibleKey({ ...selection, mode: mode as DocViewMode })));
  assert.equal(new Set(keys).size, 3);
  assert.notDeepEqual(documentVisibleKey(selection), documentVisibleKey({ ...selection, start: 0 }));
  assert.notDeepEqual(documentVisibleKey(selection), documentVisibleKey({ ...selection, base: 'other' }));
  assert.notDeepEqual(documentVisibleKey(selection), documentVisibleKey(selection, 'restart'));
});

test('details and fragments reject misbound and nonadvancing byte ranges', () => {
  const detail = { ...page, events: [page.events[1]], offset: 100 };
  assert.equal(validateDocEventDetail(detail, hash, 100).deferred, true);
  assert.throws(() => validateDocEventDetail(detail, hash, 900));
  assert.throws(() => validateDocEventDetail({ ...detail, events: [{ ...page.events[1], event_index: 900 }] }, hash, 100));
  const fragment = { event_index: 100, byte_offset: 64, next: 70, total_bytes: 100, json_fragment: '\uac00\ub098' };
  assert.equal(validateDocFragment(fragment, 100, 64), fragment);
  assert.equal(validateDocFragment({ ...fragment, next: -1, total_bytes: 70 }, 100, 64).next, -1);
  for (const invalid of [
    { ...fragment, event_index: 900 }, { ...fragment, byte_offset: 0 }, { ...fragment, next: 66 },
    { ...fragment, next: 64 }, { ...fragment, next: -1 }, { ...fragment, total_bytes: 69 },
    { ...fragment, json_fragment: '' }, { ...fragment, json_fragment: 'x'.repeat(65537) },
  ]) assert.throws(() => validateDocFragment(invalid, 100, 64));
});

test('view API, raw detail and fragment URLs never fall back to the integrity-audit endpoint', async () => {
  const previousFetch = globalThis.fetch;
  const requests: { url: URL; signal: RequestInit['signal'] }[] = [];
  let response: unknown = page;
  let status = 200;
  globalThis.fetch = async (input, options) => {
    requests.push({ url: new URL(String(input), 'https://fixture.invalid'), signal: options?.signal });
    return new Response(JSON.stringify(response), { status });
  };
  try {
    const controller = new AbortController();
    for (const mode of ['all', 'chat', 'prompts'] as const) {
      await api.getDocViewEvents('owner/repo', hash, 'sha256:base', -1, mode, controller.signal);
      const request = requests.at(-1)!;
      assert.deepEqual(Object.fromEntries(request.url.searchParams), { view: mode, offset: '-1', limit: '20', base: 'sha256:base' });
      assert.equal(request.signal, controller.signal);
      assert.equal(request.url.pathname, '/api/v1/repos/owner%2Frepo/docs/sha256%3Adocument/events');
    }
    response = { ...page, offset: 100, events: [{ ...page.events[1], event_index: 100 }] };
    await api.getDocEvent('owner/repo', hash, 100, controller.signal);
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { view: 'raw', offset: '100', limit: '1' });
    response = { event_index: 100, byte_offset: 0, next: -1, total_bytes: 2, json_fragment: '{}' };
    await api.getDocEventFragment('owner/repo', hash, 100, 0, controller.signal);
    assert.ok(requests.at(-1)!.url.pathname.endsWith('/event-fragments'));
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { index: '100', offset: '0', budget: '65536' });
    const large = { ...page, envelope: { ...page.envelope, source_models: ['x'.repeat(255 * 1024)] },
      events: [{ ...page.events[0], blocks: [{ type: 'text', text: 'v'.repeat(127 * 1024) }] }], next: 100 };
    response = large;
    assert.equal((await api.getDocViewEvents('owner/repo', hash, undefined, -1, 'chat')).envelope.source_models?.[0].length, 255 * 1024);
    assert.equal((await api.getDocEvent('owner/repo', hash, 99)).blocks?.[0].text?.length, 127 * 1024);
    for (status of [401, 409, 413, 503]) {
      response = { error: { message: 'read rejected', code: 'fixture' } };
      const before = requests.length;
      await assert.rejects(() => api.getDocEvent('owner/repo', hash, 100), ApiError);
      assert.equal(requests.length, before + 1);
      assert.equal(requests.at(-1)!.url.searchParams.get('view'), 'raw');
    }
  } finally { globalThis.fetch = previousFetch; }
});

test('fragment response budget admits a full HTML-escaped fragment without increasing decoded bytes', async () => {
  const previousFetch = globalThis.fetch;
  const fragment = { event_index: 0, byte_offset: 0, next: -1, total_bytes: 65536, json_fragment: '<'.repeat(65536) };
  const body = JSON.stringify(fragment).replaceAll('<', '\\u003c');
  assert.ok(new TextEncoder().encode(body).byteLength > 256 * 1024);
  globalThis.fetch = async () => new Response(body);
  try {
    assert.deepEqual(await api.getDocEventFragment('repo', hash, 0, 0), fragment);
    assert.throws(() => validateDocFragment({ ...fragment, total_bytes: 65537, json_fragment: '<'.repeat(65537) }, 0, 0));
  } finally { globalThis.fetch = previousFetch; }
});

test('legacy view responses still render while missing fragment support fails without an audit fallback', async () => {
  const previousFetch = globalThis.fetch;
  const requests: string[] = [];
  globalThis.fetch = async input => {
    const url = String(input);
    requests.push(url);
    if (url.includes('/event-fragments?')) return new Response('404 page not found', { status: 404 });
    return new Response(JSON.stringify({ ...page, events: [{ kind: 'tool_call', input: { command: 'legacy' } }], next: 100 }));
  };
  try {
    const legacy = await api.getDocViewEvents('repo', hash, undefined, 99, 'chat');
    assert.equal(legacy.events[0].event_index, 99);
    assert.equal(legacy.events[0].deferred, undefined);
    assert.deepEqual(legacy.events[0].input, { command: 'legacy' });
    await assert.rejects(() => api.getDocEventFragment('repo', hash, 99, 0), error => error instanceof ApiError && error.status === 404);
    assert.equal(requests.length, 2);
    assert.ok(requests[0].includes('/events?view=chat'));
    assert.ok(requests[1].includes('/event-fragments?'));
  } finally { globalThis.fetch = previousFetch; }
});
