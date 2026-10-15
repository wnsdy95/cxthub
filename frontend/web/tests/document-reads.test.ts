import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api } from '../src/api';
import { DOCUMENT_PAGE_BYTES, DocumentReadLimitError, readDocumentResponse, scheduleDocumentRead } from '../src/documentReads';

test('a 512 MiB response is cancelled after the bounded browser read budget', async () => {
  const total = 512 * 1024 * 1024;
  let produced = 0;
  let cancelled = false;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (produced === total) { controller.close(); return; }
      const bytes = new Uint8Array(65536).fill(120);
      produced += bytes.byteLength;
      controller.enqueue(bytes);
    },
    cancel() { cancelled = true; },
  });
  await assert.rejects(() => readDocumentResponse(new Response(stream), DOCUMENT_PAGE_BYTES), DocumentReadLimitError);
  assert.equal(cancelled, true);
  assert.ok(produced <= DOCUMENT_PAGE_BYTES + 2 * 65536);
  assert.ok(produced < total / 100);
});

test('shared document admission bounds active reads, cancels queued work and does not gate live reads', async () => {
  const previousFetch = globalThis.fetch;
  const releases: (() => void)[] = [];
  let active = 0;
  let maximum = 0;
  let started = 0;
  const read = () => scheduleDocumentRead(async () => {
    started++;
    active++;
    maximum = Math.max(active, maximum);
    await new Promise<void>(resolve => releases.push(resolve));
    active--;
  });
  const first = read();
  const second = read();
  const controller = new AbortController();
  let cancelledStarted = false;
  const cancelled = scheduleDocumentRead(async () => { cancelledStarted = true; }, controller.signal);
  const rejection = assert.rejects(cancelled, { name: 'AbortError' });
  const fourth = read();
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(started, 2);
  controller.abort();
  await rejection;
  globalThis.fetch = async () => new Response(JSON.stringify({ graph: '1', pending: '2' }));
  try {
    assert.deepEqual(await api.repositoryRevision('repo'), { graph: '1', pending: '2' });
  } finally { globalThis.fetch = previousFetch; }
  releases.shift()!();
  await first;
  await new Promise(resolve => setTimeout(resolve, 0));
  assert.equal(started, 3);
  while (releases.length) releases.shift()!();
  await Promise.all([second, fourth]);
  assert.equal(maximum, 2);
  assert.equal(cancelledStarted, false);
  await assert.rejects(() => scheduleDocumentRead(async () => { throw new Error('failed'); }), /failed/);
  assert.equal(await scheduleDocumentRead(async () => 'released'), 'released');
});
