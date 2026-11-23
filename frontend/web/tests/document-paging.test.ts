import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError } from '../src/api';
import { validateAnchoredEventPage, validateDocFragment, type AnchoredEventPage } from '../src/documentPaging';

const page: AnchoredEventPage = {
  offset: 99, next: 101,
  events: [
    { kind: 'message', role: 'user', seq: 400, blocks: [{ type: 'text', text: 'prompt' }] },
    { kind: 'tool_call', seq: 900, tool_name: 'Bash', deferred: true, event_bytes: 512 * 1024 * 1024 },
  ],
};

test('a page of a session conversation keeps absolute indices and rejects a range that does not advance', () => {
  const result = validateAnchoredEventPage(page, 99, 20);
  assert.deepEqual(result.events.map(event => event.event_index), [99, 100]);
  assert.equal(result.events[1].deferred, true);
  assert.equal(page.events[0].event_index, undefined);
  for (const invalid of [
    { ...page, next: 99 }, { ...page, next: 100 }, { ...page, next: 102 },
    { ...page, events: [{ ...page.events[0], event_index: 400 }, page.events[1]] },
    { ...page, events: Array.from({ length: 21 }, () => page.events[1]), next: 120 },
    { ...page, events: [], next: 99 },
  ]) assert.throws(() => validateAnchoredEventPage(invalid, 99, 20));
  assert.throws(() => validateAnchoredEventPage(page, 0, 20));
  assert.equal(validateAnchoredEventPage({ events: [], offset: 102, next: -1 }, 102, 20).next, -1);
  assert.equal(validateAnchoredEventPage({ ...page, next: -1 }, 99, 20).next, -1);
});

test('fragments reject misbound and nonadvancing byte ranges', () => {
  const fragment = { event_index: 100, byte_offset: 64, next: 70, total_bytes: 100, json_fragment: '\uac00\ub098' };
  assert.equal(validateDocFragment(fragment, 100, 64), fragment);
  assert.equal(validateDocFragment({ ...fragment, next: -1, total_bytes: 70 }, 100, 64).next, -1);
  for (const invalid of [
    { ...fragment, event_index: 900 }, { ...fragment, byte_offset: 0 }, { ...fragment, next: 66 },
    { ...fragment, next: 64 }, { ...fragment, next: -1 }, { ...fragment, total_bytes: 69 },
    { ...fragment, json_fragment: '' }, { ...fragment, json_fragment: 'x'.repeat(65537) },
  ]) assert.throws(() => validateDocFragment(invalid, 100, 64));
});

test('a conversation is read by its session: pages, one record in full, and a part of one record', async () => {
  const previousFetch = globalThis.fetch;
  const requests: { url: URL; signal: RequestInit['signal'] }[] = [];
  let response: unknown = page;
  let status = 200;
  globalThis.fetch = async (input, options) => {
    requests.push({ url: new URL(String(input), 'https://fixture.invalid'), signal: options?.signal });
    return new Response(JSON.stringify(response), { status });
  };
  const base = '/api/v1/repos/owner%2Frepo/anchored-sessions/claude/session%2Fone';
  try {
    const controller = new AbortController();
    const read = await api.anchoredEvents('owner/repo', 'claude', 'session/one', 99, controller.signal);
    assert.deepEqual(read.events.map(event => event.event_index), [99, 100]);
    assert.equal(requests.at(-1)!.url.pathname, `${base}/events`);
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { offset: '99', limit: '20' });
    assert.equal(requests.at(-1)!.signal, controller.signal);

    response = { offset: 100, next: 101, events: [{ ...page.events[1], event_index: 100 }] };
    assert.equal((await api.anchoredEvent('owner/repo', 'claude', 'session/one', 100, controller.signal)).tool_name, 'Bash');
    assert.equal(requests.at(-1)!.url.pathname, `${base}/events`);
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { view: 'raw', offset: '100', limit: '1' });
    response = { offset: 100, next: -1, events: [] };
    await assert.rejects(() => api.anchoredEvent('owner/repo', 'claude', 'session/one', 100), /Invalid session conversation event/);

    response = { event_index: 100, byte_offset: 0, next: -1, total_bytes: 2, json_fragment: '{}' };
    await api.anchoredEventFragment('owner/repo', 'claude', 'session/one', 100, 0, controller.signal);
    assert.equal(requests.at(-1)!.url.pathname, `${base}/event-fragments`);
    assert.deepEqual(Object.fromEntries(requests.at(-1)!.url.searchParams), { index: '100', offset: '0', budget: '65536' });

    for (status of [401, 404, 409, 413, 503]) {
      response = { error: { message: 'read rejected', code: 'fixture' } };
      const before = requests.length;
      await assert.rejects(() => api.anchoredEvent('owner/repo', 'claude', 'session/one', 100), ApiError);
      assert.equal(requests.length, before + 1);
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
    assert.deepEqual(await api.anchoredEventFragment('repo', 'claude', 'session', 0, 0), fragment);
    assert.throws(() => validateDocFragment({ ...fragment, total_bytes: 65537, json_fragment: '<'.repeat(65537) }, 0, 0));
  } finally { globalThis.fetch = previousFetch; }
});
