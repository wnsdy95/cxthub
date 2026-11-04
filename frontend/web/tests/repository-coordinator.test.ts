import assert from 'node:assert/strict';
import { test, type TestContext } from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { api, ApiError, ApiTransportError } from '../src/api';
import { readRepositoryPending, startRepositoryUpdates } from '../src/useRepositoryUpdates';
import { parseRevision } from '../src/repositoryUpdates';
import { pendingStateRows } from '../src/pendingState';
import type { PendingStateView, PendingView, RepositoryRevision, RepositoryView } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture } from './pendingViewFixture';

const fullKey = ['repo-view', 'repo'];
const pendingKey = ['pending-state', 'repo', 'on-hold-v1'];
const noticeKey = ['repository-notice', 'repo'];
const flush = () => new Promise<void>(resolve => setImmediate(resolve));

function deferred<Value>() {
  let resolve!: (value: Value) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<Value>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function setup(context: TestContext, revision: RepositoryRevision) {
  context.mock.timers.enable({ apis: ['setTimeout'] });
  context.mock.method(Math, 'random', () => 0);
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document');
  const originalSource = Object.getOwnPropertyDescriptor(globalThis, 'EventSource');
  const document = Object.assign(new EventTarget(), { hidden: false });
  const sources: EventTarget[] = [];
  class Source extends EventTarget {
    constructor() { super(); sources.push(this); }
    close() { sources.splice(sources.indexOf(this), 1); }
  }
  Object.assign(globalThis, { document, EventSource: Source });
  const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity, retry: false } } });
  client.setQueryData(fullKey, fullFixture(pendingFixture('old', revision)));
  client.setQueryData(pendingKey, pendingStateFixture(pendingFixture('old', revision)));
  let stop: (() => void) | undefined;
  context.after(() => {
    stop?.();
    client.clear();
    for (const [key, descriptor] of [['document', originalDocument], ['EventSource', originalSource]] as const) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
  return {
    client,
    start: () => { stop = startRepositoryUpdates('repo', client); },
    stop: () => stop?.(),
    hide: () => { document.hidden = true; document.dispatchEvent(new Event('visibilitychange')); },
    reconnect: () => document.dispatchEvent(new Event('visibilitychange')),
    emit: (next: RepositoryRevision) => {
      for (const source of sources) source.dispatchEvent(new MessageEvent('revision', { data: JSON.stringify(next) }));
    },
  };
}

function durable(target: string, revision: RepositoryRevision, incarnation = '12345678-abcd-4321-9876-123456789abc'): PendingStateView {
  return { ...pendingStateFixture(pendingFixture(target, revision)), read_generation: {
    generation: `sha256:${'a'.repeat(64)}`, source_epoch: `${incarnation}:1`, applied_epoch: `${incarnation}:1`, rules: 1, complete: true,
  } };
}

function readyNotice(revision: RepositoryRevision, sequence = '2', digest = 'b', incarnation = '12345678-abcd-4321-9876-123456789abc'): RepositoryRevision {
  return { ...revision, pending_projection: {
    state: 'ready', source_epoch: `${incarnation}:${sequence}`, rules: 1, generation: `sha256:${digest.repeat(64)}`,
  } };
}

function published(target: string, notice: RepositoryRevision): PendingStateView {
  const status = notice.pending_projection!;
  return { ...durable(target, notice), read_generation: {
    generation: status.generation!, source_epoch: status.source_epoch, applied_epoch: status.source_epoch, rules: 1, complete: true,
  } };
}

test('optional projection notices validate canonical states, generations and signed64 source epochs', () => {
  const revision = { graph: '1', pending: '2' };
  assert.equal(parseRevision(revision), revision);
  const additive = { ...readyNotice(revision), pending_projection: { ...readyNotice(revision).pending_projection, lag_metric: 3 } };
  assert.equal(parseRevision(additive), additive);
  for (const sequence of ['0', '1', '9007199254740993', '9223372036854775807']) {
    const ready = readyNotice(revision, sequence);
    for (const state of ['disabled', 'catching_up', 'needs_attention', 'ready'] as const) {
      const { generation, ...status } = ready.pending_projection!;
      const notice = { ...revision, pending_projection: { ...status, state, ...(state === 'ready' ? { generation } : {}) } };
      assert.equal(parseRevision(notice), notice);
    }
  }
});

test('optional projection notices reject malformed metadata instead of accepting only revision counters', () => {
  const ready = readyNotice({ graph: '1', pending: '2' });
  const status = ready.pending_projection!;
  const invalid: unknown[] = [null, false, 1, 'ready', [], {}];
  const fields: Record<string, unknown[]> = {
    state: [undefined, null, false, 1, 'pending', 'READY', 'ready\n'],
    rules: [undefined, null, '1', 0, 2, true],
    generation: [undefined, null, 1, '', 'b'.repeat(64), `sha256:${'b'.repeat(63)}`, `sha256:${'b'.repeat(65)}`,
      `sha256:${'B'.repeat(64)}`, `sha256:${'g'.repeat(64)}`, `${status.generation}\n`],
    source_epoch: [undefined, null, 1, '', status.source_epoch.toUpperCase(), status.source_epoch.replaceAll('-', ''),
      ...['', '-1', '+1', '01', '1.0', '1e2', '1 ', '１', '9223372036854775808', '9'.repeat(100)]
        .map(sequence => `12345678-abcd-4321-9876-123456789abc:${sequence}`)],
  };
  for (const [field, values] of Object.entries(fields)) for (const value of values) invalid.push({ ...status, [field]: value });
  for (const state of ['disabled', 'catching_up', 'needs_attention']) {
    invalid.push({ ...status, state }, { ...status, state, generation: undefined });
  }
  for (const suffix of ['\n', '\r', '\u2028', '\u2029']) {
    invalid.push({ ...status, source_epoch: status.source_epoch + suffix }, { ...status, generation: status.generation + suffix });
  }
  for (const pending_projection of invalid) assert.equal(parseRevision({ ...ready, pending_projection }), null, JSON.stringify(pending_projection));
});

test('invalid HTTP and SSE status notices cannot update the notice cache or trigger bodies', async context => {
  const revision = { graph: '1', pending: '1' };
  const invalid = { ...readyNotice(revision), pending_projection: { ...readyNotice(revision).pending_projection, rules: 2 } };
  const fixture = setup(context, revision);
  context.mock.method(api, 'repositoryRevision', async () => invalid as RepositoryRevision);
  const pending = context.mock.method(api, 'pendingState', async () => durable('old', revision));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  fixture.emit(invalid as RepositoryRevision);
  await flush();
  assert.equal(fixture.client.getQueryData(noticeKey), undefined);
  assert.equal(pending.mock.callCount(), 0);
  assert.equal(full.mock.callCount(), 0);
});

for (const status of [401, 403]) {
  test(`revision ${status} latches access denial across SSE until an authorized status forces fresh bodies`, async context => {
    const revision = { graph: '1', pending: '1' };
    const notice = readyNotice(revision);
    const fixture = setup(context, revision);
    fixture.client.setQueryData(pendingKey, published('cached', notice));
    let denied = false;
    context.mock.method(api, 'repositoryRevision', async () => {
      if (denied) throw new ApiError('Access denied', 'forbidden', status);
      return notice;
    });
    const pending = context.mock.method(api, 'pendingState', async () => published('unexpected', notice));
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
    const rows = () => pendingStateRows(fixture.client.getQueryData(fullKey), fixture.client.getQueryData(pendingKey), {
      projection: fixture.client.getQueryData<RepositoryRevision>(noticeKey)?.pending_projection,
      noticeError: fixture.client.getQueryState<RepositoryRevision, Error>(noticeKey)?.error,
    });
    fixture.start();
    await flush();
    assert.equal(rows().ready, true);
    denied = true;
    fixture.reconnect();
    await flush();
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'error');
    assert.equal(rows().inaccessible, true);
    assert.equal(rows().ready, false);
    assert.deepEqual(rows().pendings, []);
    context.mock.timers.tick(15000);
    await flush();
    assert.equal(rows().inaccessible, true);
    denied = false;
    fixture.emit(notice);
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'error');
    assert.equal(rows().inaccessible, true);
    await flush();
    assert.equal(pending.mock.callCount(), 0);
    assert.equal(full.mock.callCount(), 0);
    fixture.reconnect();
    await flush();
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'success');
    assert.equal(rows().inaccessible, false);
    assert.equal(rows().ready, true);
    assert.equal(pending.mock.callCount(), 1);
    assert.equal(full.mock.callCount(), 1);
  });
}

test('an initial revision denial publishes access state without requiring a prior cached notice', async context => {
  const revision = { graph: '1', pending: '1' };
  const fixture = setup(context, revision);
  const denied = new ApiError('Access denied', 'forbidden', 403);
  context.mock.method(api, 'repositoryRevision', async () => { throw denied; });
  fixture.start();
  await flush();
  assert.equal(fixture.client.getQueryData(noticeKey), undefined);
  assert.equal(fixture.client.getQueryState(noticeKey)?.error, denied);
  const rows = pendingStateRows(fixture.client.getQueryData(fullKey), fixture.client.getQueryData(pendingKey), { noticeError: denied });
  assert.equal(rows.inaccessible, true);
  assert.equal(rows.ready, false);
});

test('ready source-only notices fetch pending once and publish stale UI state before the response', async context => {
  const revision = { graph: '1', pending: '1' };
  const notice = readyNotice(revision);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  const response = deferred<PendingStateView>();
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', () => response.promise);
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  const patch = context.mock.method(api, 'pendingView', async () => pendingFixture());
  fixture.start();
  await flush();
  assert.deepEqual(fixture.client.getQueryData(noticeKey), notice);
  const rows = () => pendingStateRows(fixture.client.getQueryData(fullKey), fixture.client.getQueryData(pendingKey), {
    projection: fixture.client.getQueryData<RepositoryRevision>(noticeKey)?.pending_projection,
  });
  assert.equal(rows().ready, false);
  assert.equal(rows().refreshing, true);
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  response.resolve(published('published', notice));
  await flush();
  assert.equal(rows().ready, true);
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  context.mock.timers.tick(120000);
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 0);
  assert.equal(patch.mock.callCount(), 0);
});

for (const durableCache of [false, true]) {
  test(`disabled source tokens refresh ${durableCache ? 'durable' : 'legacy'} cache once per token or mode change`, async context => {
    const revision = { graph: '1', pending: '1' };
    const fixture = setup(context, revision);
    if (durableCache) fixture.client.setQueryData(pendingKey, durable('cached', revision));
    let notice: RepositoryRevision = { ...revision, pending_projection: {
      state: 'disabled', source_epoch: '12345678-abcd-4321-9876-123456789abc:1', rules: 1,
    } };
    context.mock.method(api, 'repositoryRevision', async () => notice);
    const pending = context.mock.method(api, 'pendingState', async () => pendingStateFixture(pendingFixture(`read-${pending.mock.callCount()}`, revision)));
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
    fixture.start();
    await flush();
    assert.equal(pending.mock.callCount(), 1);
    fixture.emit(notice);
    fixture.reconnect();
    await flush();
    assert.equal(pending.mock.callCount(), 1);
    notice = { ...notice, pending_projection: { ...notice.pending_projection!, source_epoch: '12345678-abcd-4321-9876-123456789abc:2' } };
    fixture.emit(notice);
    await flush();
    assert.equal(pending.mock.callCount(), 2);
    assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'read-1');
    const disabled = notice;
    notice = { ...revision, pending_projection: { state: 'catching_up', source_epoch: disabled.pending_projection!.source_epoch, rules: 1 } };
    fixture.emit(notice);
    await flush();
    assert.equal(pending.mock.callCount(), 2);
    notice = disabled;
    fixture.emit(notice);
    await flush();
    assert.equal(pending.mock.callCount(), 3);
    fixture.emit(notice);
    fixture.reconnect();
    await flush();
    context.mock.timers.tick(120000);
    await flush();
    assert.equal(pending.mock.callCount(), 3);
    assert.equal(full.mock.callCount(), 0);
  });
}

test('manual pending retries keep an actionable generation error until a matching response succeeds', async context => {
  const revision = { graph: '1', pending: '1' };
  const notice = readyNotice(revision);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  let matching = false;
  const pending = context.mock.method(api, 'pendingState', async () => matching ? published('matching', notice) : durable('stale', revision));
  fixture.start();
  await flush();
  context.mock.timers.tick(15000);
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  const manualRetry = () => fixture.client.fetchQuery({ queryKey: pendingKey, retry: false,
    queryFn: ({ signal }) => readRepositoryPending('repo', fixture.client, signal) });
  await assert.rejects(manualRetry(), /does not match the published generation/);
  assert.equal(pending.mock.callCount(), 3);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'error');
  assert.match(fixture.client.getQueryState(pendingKey)?.error?.message ?? '', /Retry the read/);
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'cached');
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 3);
  matching = true;
  await manualRetry();
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'success');
  assert.equal(fixture.client.getQueryState(pendingKey)?.error, null);
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'matching');
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 4);
});

test('matching ready generations need no pending body even when graph counters differ', async context => {
  const revision = { graph: '1', pending: '1' };
  const notice = readyNotice({ graph: '2', pending: '2' });
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, { ...published('cached', notice), revision });
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => published('unexpected', notice));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('new', notice)));
  fixture.start();
  await flush();
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 0);
  assert.equal(full.mock.callCount(), 1);
});

test('status transitions recover terminal pending failures without identical-notice body polling', async context => {
  const revision = { graph: '1', pending: '1' };
  const ready = readyNotice(revision);
  const { generation: _generation, ...status } = ready.pending_projection!;
  let notice: RepositoryRevision = { ...revision, pending_projection: { ...status, state: 'catching_up' } };
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  await assert.rejects(fixture.client.fetchQuery({ queryKey: pendingKey, retry: false, queryFn: async () => {
    throw new ApiError('requires attention', 'pending_projection_failed', 503);
  } }));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => {
    if (notice.pending_projection?.state !== 'ready') throw new ApiError('requires attention', 'pending_projection_failed', 503);
    return published('ready', ready);
  });
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  assert.equal(pending.mock.callCount(), 0);
  for (const delay of [15000, 30000, 60000, 120000]) {
    context.mock.timers.tick(delay);
    await flush();
  }
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 0);
  notice = { ...notice, pending_projection: { ...status, state: 'needs_attention' } };
  fixture.emit(notice);
  assert.deepEqual(fixture.client.getQueryData(noticeKey), notice);
  await flush();
  assert.equal(pending.mock.callCount(), 0);
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 0);
  notice = ready;
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'success');
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'ready');
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 0);
});

test('ready notices retry a terminal query even when its cached generation already matches', async context => {
  const revision = { graph: '1', pending: '1' };
  const notice = readyNotice(revision);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, published('cached', notice));
  await assert.rejects(fixture.client.fetchQuery({ queryKey: pendingKey, retry: false, queryFn: async () => {
    throw new ApiError('requires attention', 'pending_projection_failed', 503);
  } }));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => published('recovered', notice));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'success');
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 0);
});

test('metadata-only changes during a pending flight drain the latest ready generation immediately', async context => {
  const revision = { graph: '1', pending: '1' };
  const first = readyNotice(revision);
  const latest = readyNotice(revision, '3', 'c');
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  const response = deferred<PendingStateView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
    ? response.promise : Promise.resolve(published('latest', latest)));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  fixture.emit(latest);
  fixture.emit(latest);
  response.resolve(published('first', first));
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(full.mock.callCount(), 0);
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.read_generation?.generation, latest.pending_projection?.generation);
});

test('an unchanged ready notice cannot spin or repeatedly fetch a lagging pending replica', async context => {
  const revision = { graph: '1', pending: '1' };
  let notice = readyNotice(revision);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => durable('old replica', revision));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  for (const delay of [15000, 30000, 60000, 120000]) {
    context.mock.timers.tick(delay);
    await flush();
  }
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'error');
  notice = readyNotice(revision, '3', 'c');
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 3);
  assert.equal(full.mock.callCount(), 0);
});

test('source UUID notices refresh full once at unchanged counters, unlike sequence-only notices', async context => {
  const revision = { graph: '1', pending: '1' };
  const incarnation = '22345678-abcd-4321-9876-123456789abc';
  let notice = readyNotice(revision, '0', 'b', incarnation);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => published('reset', notice));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('reset', revision)));
  fixture.start();
  await flush();
  assert.equal(full.mock.callCount(), 1);
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(pending.mock.callCount(), 1);
  notice = readyNotice(revision, '1', 'c', incarnation);
  fixture.emit(notice);
  await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(pending.mock.callCount(), 2);
});

test('a UUID reset during a full flight queues one full followup even at identical counters', async context => {
  const previous = { graph: '1', pending: '1' };
  const revision = { graph: '2', pending: '2' };
  const first = readyNotice(revision);
  const latest = readyNotice(revision, '0', 'c', '22345678-abcd-4321-9876-123456789abc');
  const fixture = setup(context, previous);
  fixture.client.setQueryData(pendingKey, durable('cached', previous));
  const response = deferred<RepositoryView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  context.mock.method(api, 'pendingState', async () => published('latest', latest));
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0
    ? response.promise : Promise.resolve(fullFixture(pendingFixture('reset', revision))));
  fixture.start();
  await flush();
  fixture.emit(latest);
  fixture.emit(latest);
  assert.equal(full.mock.callCount(), 1);
  response.resolve(fullFixture(pendingFixture('old incarnation', revision)));
  await flush();
  assert.equal(full.mock.callCount(), 2);
  fixture.emit(latest);
  await flush();
  assert.equal(full.mock.callCount(), 2);
});

for (const change of ['revision', 'generation', 'incarnation'] as const) {
  test(`a ${change} notice joining an observer-owned flight gets one fresh followup after its older response`, async context => {
    const previous = { graph: '1', pending: '1' };
    const notice = change === 'revision' ? { graph: '2', pending: '2' }
      : readyNotice(previous, '2', 'b', change === 'incarnation'
        ? '22345678-abcd-4321-9876-123456789abc' : '12345678-abcd-4321-9876-123456789abc');
    const fixture = setup(context, previous);
    fixture.client.setQueryData(pendingKey, durable('cached', previous));
    const fullResponse = deferred<RepositoryView>();
    const pendingResponse = deferred<PendingStateView>();
    const observerFull = fixture.client.fetchQuery({ queryKey: fullKey, queryFn: () => fullResponse.promise });
    const observerPending = fixture.client.fetchQuery({ queryKey: pendingKey, queryFn: () => pendingResponse.promise });
    context.mock.method(api, 'repositoryRevision', async () => notice);
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('latest', notice)));
    const pending = context.mock.method(api, 'pendingState', async () => notice.pending_projection
      ? published('latest', notice) : durable('latest', notice));
    fixture.start();
    await flush();
    fixture.emit(notice);
    await flush();
    assert.equal(full.mock.callCount(), 0);
    assert.equal(pending.mock.callCount(), 0);
    fullResponse.resolve(fullFixture(pendingFixture('observer', previous)));
    pendingResponse.resolve(durable('observer', previous));
    await Promise.all([observerFull, observerPending]);
    await flush();
    assert.equal(pending.mock.callCount(), 1);
    assert.equal(full.mock.callCount(), change === 'generation' ? 0 : 1);
    fixture.emit(notice);
    await flush();
    assert.equal(pending.mock.callCount(), 1);
    assert.equal(full.mock.callCount(), change === 'generation' ? 0 : 1);
  });
}

test('live coordinator retries pending projection lag independently of full graph reads', async context => {
  const previous = { graph: '1', pending: '1' };
  const next = { graph: '1', pending: '2' };
  const fixture = setup(context, previous);
  fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('full', next)));
  context.mock.method(api, 'repositoryRevision', async () => next);
  let reads = 0;
  context.mock.method(api, 'pendingState', async () => {
    if (++reads <= 2) throw new ApiError('catching up', 'pending_projection_pending', 503, 3000);
    return durable('ready', next);
  });
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  assert.equal(reads, 1);
  assert.equal(fixture.client.getQueryState(pendingKey)?.fetchFailureCount, 1);
  fixture.emit(next);
  fixture.emit(next);
  await flush();
  assert.equal(reads, 1);
  context.mock.timers.tick(2999);
  await flush();
  assert.equal(reads, 1);
  context.mock.timers.tick(1);
  await flush();
  assert.equal(reads, 2);
  context.mock.timers.tick(3000);
  await flush();
  assert.equal(reads, 3);
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'ready');
  assert.equal(full.mock.callCount(), 0);
});

for (const compact of [false, true]) {
  test(`access denial cancels payload queries and fences late ${compact ? 'compact merges' : 'full responses'}`, async context => {
    const previous = { graph: '1', pending: '1' };
    const next = { graph: compact ? '1' : '2', pending: '2' };
    const fixture = setup(context, previous);
    const fullResponse = deferred<RepositoryView>();
    const patchResponse = deferred<PendingView>();
    const pendingResponse = deferred<PendingStateView>();
    let forbidden = false;
    context.mock.method(api, 'repositoryRevision', async () => {
      if (forbidden) throw new ApiError('Access denied', 'forbidden', 403);
      return next;
    });
    const full = context.mock.method(api, 'repositoryView', () => !compact && full.mock.callCount() === 0
      ? fullResponse.promise : Promise.resolve(fullFixture(pendingFixture('authorized', next))));
    const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
      ? pendingResponse.promise : Promise.resolve(pendingStateFixture(pendingFixture('authorized', next))));
    const patch = context.mock.method(api, 'pendingView', () => patchResponse.promise);
    fixture.start();
    await flush();
    forbidden = true;
    fixture.reconnect();
    await flush();
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'error');
    assert.equal(fixture.client.getQueryState(fullKey)?.fetchStatus, 'idle');
    assert.equal(fixture.client.getQueryState(pendingKey)?.fetchStatus, 'idle');
    fullResponse.resolve(fullFixture(pendingFixture('unauthorized late full', next)));
    pendingResponse.resolve(pendingStateFixture(pendingFixture('unauthorized late pending', next)));
    patchResponse.resolve(pendingFixture('unauthorized late patch', next));
    await flush();
    assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.pending[0].target, 'old');
    assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'old');
    fixture.emit(next);
    await flush();
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'error');
    assert.equal(pending.mock.callCount(), 1);
    forbidden = false;
    fixture.reconnect();
    await flush();
    assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'success');
    assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.pending[0].target, 'authorized');
    assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.pending[0].target, 'authorized');
    assert.equal(full.mock.callCount(), compact ? 1 : 2);
    assert.equal(pending.mock.callCount(), 2);
    assert.equal(patch.mock.callCount(), compact ? 1 : 0);
  });
}

for (const recoverable of [false, true]) {
  test(`same-counter UUID reset ${recoverable ? 'acknowledges only a successful repair' : 'stops after a bounded failed repair'}`, async context => {
    const revision = { graph: '1', pending: '1' };
    const notice = readyNotice(revision, '1', 'b', '22345678-abcd-4321-9876-123456789abc');
    const fixture = setup(context, revision);
    fixture.client.setQueryData(pendingKey, durable('cached', revision));
    context.mock.method(api, 'repositoryRevision', async () => notice);
    const pending = context.mock.method(api, 'pendingState', async () => published('new incarnation', notice));
    const full = context.mock.method(api, 'repositoryView', async () => {
      if (!recoverable || full.mock.callCount() === 0) throw new ApiTransportError('disconnected');
      return fullFixture(pendingFixture('new incarnation', revision));
    });
    fixture.start();
    await flush();
    assert.equal(full.mock.callCount(), 1);
    assert.equal(fixture.client.getQueryState(fullKey)?.status, 'error');
    for (const delay of [15000, 30000, 60000, 120000]) {
      context.mock.timers.tick(delay);
      await flush();
    }
    fixture.emit(notice);
    fixture.reconnect();
    await flush();
    assert.equal(full.mock.callCount(), 2);
    assert.equal(pending.mock.callCount(), 1);
    assert.equal(fixture.client.getQueryState(fullKey)?.status, recoverable ? 'success' : 'error');
    assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.pending[0].target, recoverable ? 'new incarnation' : 'old');
  });
}

test('an identical ready notice can repair one failed attempt without unlimited body retries', async context => {
  const revision = { graph: '1', pending: '1' };
  const notice = readyNotice(revision);
  const fixture = setup(context, revision);
  fixture.client.setQueryData(pendingKey, durable('cached', revision));
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => {
    if (pending.mock.callCount() === 0) throw new ApiError('requires attention', 'pending_projection_failed', 503);
    return published('repaired', notice);
  });
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'success');
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(full.mock.callCount(), 0);
});

test('catching-up metadata cancels legacy preparation retries and gates bodies until ready', async context => {
  const previous = { graph: '1', pending: '1' };
  const next = { graph: '1', pending: '2' };
  const ready = readyNotice(next);
  let notice: RepositoryRevision = next;
  const fixture = setup(context, previous);
  fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('new', next)));
  const { generation: _generation, ...status } = ready.pending_projection!;
  context.mock.method(api, 'repositoryRevision', async () => notice);
  const pending = context.mock.method(api, 'pendingState', async () => {
    if (notice.pending_projection?.state !== 'ready') throw new ApiError('catching up', 'pending_projection_pending', 503, 1000);
    return published('ready', ready);
  });
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  fixture.start();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  notice = { ...next, pending_projection: { ...status, state: 'catching_up' } };
  fixture.emit(notice);
  await flush();
  assert.equal(fixture.client.getQueryState(pendingKey)?.fetchStatus, 'idle');
  for (const delay of [1000, 2000, 3000, 90000]) {
    context.mock.timers.tick(delay);
    await flush();
  }
  fixture.emit(notice);
  fixture.reconnect();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  notice = ready;
  fixture.emit(notice);
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryState(pendingKey)?.status, 'success');
  assert.equal(full.mock.callCount(), 0);
});

for (const next of [
  { graph: '1', pending: '1', evidence: '1' },
  { graph: '101', pending: '99', evidence: '101' },
  { graph: '100', pending: '99', evidence: '100' },
  { graph: '100', pending: '100', evidence: '99' },
]) {
  test(`lower or incomparable revision notice refreshes both reads (${JSON.stringify(next)})`, async context => {
    const previous = { graph: '100', pending: '100', evidence: '100' };
    const fixture = setup(context, previous);
    context.mock.method(api, 'repositoryRevision', async () => previous);
    const pending = context.mock.method(api, 'pendingState', async () => durable('new', next));
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('new', next)));
    const patch = context.mock.method(api, 'pendingView', async () => pendingFixture());
    fixture.start();
    await flush();
    assert.equal(pending.mock.callCount(), 0);
    fixture.emit(next);
    await flush();
    assert.equal(pending.mock.callCount(), 1);
    assert.equal(full.mock.callCount(), 1);
    assert.equal(patch.mock.callCount(), 0);
    assert.deepEqual(fixture.client.getQueryData<PendingStateView>(pendingKey)?.revision, next);
    assert.deepEqual(fixture.client.getQueryData<{ revision: RepositoryRevision }>(fullKey)?.revision, next);
  });
}

test('a mismatching replica gets one refresh pass before scheduled recovery, not a synchronous loop', async context => {
  const previous = { graph: '1', pending: '1' };
  const wanted = { graph: '2', pending: '2' };
  const fixture = setup(context, previous);
  context.mock.method(api, 'repositoryRevision', async () => wanted);
  const pending = context.mock.method(api, 'pendingState', async () => pendingStateFixture(pendingFixture('old', previous)));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('old', previous)));
  fixture.start();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 1);
  context.mock.timers.tick(14999);
  await flush();
  assert.equal(full.mock.callCount(), 1);
  context.mock.timers.tick(1);
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(full.mock.callCount(), 2);
});

for (const graphChanges of [true, false]) {
  test(`in-flight ${graphChanges ? 'full' : 'compact'} reads coalesce distinct notices into an immediate latest-wanted pass`, async context => {
    const previous = { graph: '1', pending: '1' };
    const first = { graph: graphChanges ? '2' : '1', pending: '2' };
    const middle = { graph: graphChanges ? '3' : '1', pending: '3' };
    const latest = { graph: graphChanges ? '4' : '1', pending: '4' };
    const fixture = setup(context, previous);
    const firstView = deferred<PendingView>();
    const firstPending = deferred<PendingStateView>();
    const revision = context.mock.method(api, 'repositoryRevision', async () => first);
    const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
      ? firstPending.promise : Promise.resolve(pendingStateFixture(pendingFixture('latest', latest))));
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture(full.mock.callCount() === 0
      ? await firstView.promise : pendingFixture('latest', latest)));
    const patch = context.mock.method(api, 'pendingView', () => patch.mock.callCount() === 0
      ? firstView.promise : Promise.resolve(pendingFixture('latest', latest)));
    const view = graphChanges ? full : patch;
    fixture.start();
    await flush();
    fixture.emit(first);
    fixture.emit(middle);
    fixture.emit(latest);
    fixture.emit({ ...latest, evidence: '0' });
    await flush();
    assert.equal(view.mock.callCount(), 1);
    assert.equal(pending.mock.callCount(), 1);
    firstView.resolve(pendingFixture('first', first));
    firstPending.resolve(pendingStateFixture(pendingFixture('first', first)));
    await flush();
    assert.equal(view.mock.callCount(), 2);
    assert.equal(pending.mock.callCount(), 2);
    assert.deepEqual(fixture.client.getQueryData<RepositoryView>(fullKey)?.revision, latest);
    assert.deepEqual(fixture.client.getQueryData<PendingStateView>(pendingKey)?.revision, latest);
    assert.equal(revision.mock.callCount(), 1);
    if (!graphChanges) assert.equal(full.mock.callCount(), 0);
    fixture.emit(latest);
    fixture.emit({ ...latest, evidence: '0' });
    await flush();
    assert.equal(view.mock.callCount(), 2);
    assert.equal(pending.mock.callCount(), 2);
  });
}

test('a distinct notice during pending-only reads is followed immediately without fetching full bodies', async context => {
  const previous = { graph: '1', pending: '1' };
  const first = { graph: '1', pending: '2' };
  const latest = { graph: '1', pending: '3' };
  const fixture = setup(context, previous);
  fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('first', first)));
  const response = deferred<PendingStateView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
    ? response.promise : Promise.resolve(pendingStateFixture(pendingFixture('latest', latest))));
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture());
  const patch = context.mock.method(api, 'pendingView', async () => pendingFixture());
  fixture.start();
  await flush();
  fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('latest', latest)));
  fixture.emit(latest);
  fixture.emit(latest);
  response.resolve(pendingStateFixture(pendingFixture('first', first)));
  await flush();
  assert.equal(pending.mock.callCount(), 2);
  assert.equal(full.mock.callCount(), 0);
  assert.equal(patch.mock.callCount(), 0);
  assert.deepEqual(fixture.client.getQueryData<PendingStateView>(pendingKey)?.revision, latest);
});

for (const distinctNotice of [false, true]) {
  test(`lagging deferred responses allow ${distinctNotice ? 'one distinct-notice followup' : 'no duplicate-notice followup'} before recovery`, async context => {
    const previous = { graph: '1', pending: '1' };
    const first = { graph: '2', pending: '2' };
    const latest = distinctNotice ? { graph: '3', pending: '3' } : first;
    const fixture = setup(context, previous);
    const firstFull = deferred<RepositoryView>();
    const firstPending = deferred<PendingStateView>();
    context.mock.method(api, 'repositoryRevision', async () => latest);
    const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0
      ? firstFull.promise : Promise.resolve(fullFixture(pendingFixture('old', previous))));
    const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
      ? firstPending.promise : Promise.resolve(pendingStateFixture(pendingFixture('old', previous))));
    fixture.start();
    fixture.emit(first);
    await flush();
    fixture.emit(latest);
    fixture.emit({ ...latest, evidence: '0' });
    firstFull.resolve(fullFixture(pendingFixture('old', previous)));
    firstPending.resolve(pendingStateFixture(pendingFixture('old', previous)));
    await flush();
    const expected = distinctNotice ? 2 : 1;
    assert.equal(full.mock.callCount(), expected);
    assert.equal(pending.mock.callCount(), expected);
    context.mock.timers.tick(14999);
    await flush();
    assert.equal(full.mock.callCount(), expected);
    assert.equal(pending.mock.callCount(), expected);
    context.mock.timers.tick(1);
    await flush();
    assert.equal(full.mock.callCount(), expected + 1);
    assert.equal(pending.mock.callCount(), expected + 1);
  });
}

test('responses already matching the latest in-flight notice need no followup or recurring body reads', async context => {
  const previous = { graph: '1', pending: '1' };
  const first = { graph: '2', pending: '2' };
  const latest = { graph: '3', pending: '3' };
  const fixture = setup(context, previous);
  const firstFull = deferred<RepositoryView>();
  const firstPending = deferred<PendingStateView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const full = context.mock.method(api, 'repositoryView', () => firstFull.promise);
  const pending = context.mock.method(api, 'pendingState', () => firstPending.promise);
  fixture.start();
  await flush();
  fixture.emit(latest);
  firstFull.resolve(fullFixture(pendingFixture('latest', latest)));
  firstPending.resolve(pendingStateFixture(pendingFixture('latest', latest)));
  await flush();
  fixture.emit(latest);
  context.mock.timers.tick(120000);
  await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(pending.mock.callCount(), 1);
});

test('a failed in-flight request still drains a distinct latest notice immediately', async context => {
  const previous = { graph: '1', pending: '1' };
  const first = { graph: '2', pending: '2' };
  const latest = { graph: '3', pending: '3' };
  const fixture = setup(context, previous);
  const firstFull = deferred<RepositoryView>();
  const firstPending = deferred<PendingStateView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0
    ? firstFull.promise : Promise.resolve(fullFixture(pendingFixture('latest', latest))));
  const pending = context.mock.method(api, 'pendingState', () => pending.mock.callCount() === 0
    ? firstPending.promise : Promise.resolve(pendingStateFixture(pendingFixture('latest', latest))));
  fixture.start();
  await flush();
  fixture.emit(latest);
  firstFull.reject(new ApiTransportError('disconnected'));
  firstPending.reject(new ApiTransportError('disconnected'));
  await flush();
  assert.equal(full.mock.callCount(), 2);
  assert.equal(pending.mock.callCount(), 2);
  assert.deepEqual(fixture.client.getQueryData<RepositoryView>(fullKey)?.revision, latest);
  assert.deepEqual(fixture.client.getQueryData<PendingStateView>(pendingKey)?.revision, latest);
});

for (const lifecycle of ['hide', 'stop'] as const) {
  test(`${lifecycle} suppresses queued in-flight followups`, async context => {
    const previous = { graph: '1', pending: '1' };
    const first = { graph: '2', pending: '2' };
    const latest = { graph: '3', pending: '3' };
    const fixture = setup(context, previous);
    const firstFull = deferred<RepositoryView>();
    const firstPending = deferred<PendingStateView>();
    context.mock.method(api, 'repositoryRevision', async () => first);
    const full = context.mock.method(api, 'repositoryView', () => firstFull.promise);
    const pending = context.mock.method(api, 'pendingState', () => firstPending.promise);
    fixture.start();
    await flush();
    fixture.emit(latest);
    fixture[lifecycle]();
    firstFull.resolve(fullFixture(pendingFixture('first', first)));
    firstPending.resolve(pendingStateFixture(pendingFixture('first', first)));
    await flush();
    context.mock.timers.tick(120000);
    await flush();
    assert.equal(full.mock.callCount(), 1);
    assert.equal(pending.mock.callCount(), 1);
  });
}

test('a received durable epoch refreshes full once; duplicate notices and reconnects fetch no bodies', async context => {
  const previous = { graph: '1', pending: '1' };
  const revision = { graph: '1', pending: '2' };
  const fixture = setup(context, previous);
  fixture.client.setQueryData(pendingKey, durable('old', previous));
  fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('new', revision)));
  context.mock.method(api, 'repositoryRevision', async () => revision);
  const next = durable('new', revision, '22345678-abcd-4321-9876-123456789abc');
  const pending = context.mock.method(api, 'pendingState', async () => next);
  const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('new', revision)));
  fixture.start();
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryData<PendingStateView>(pendingKey)?.read_generation?.source_epoch, next.read_generation?.source_epoch);
  fixture.emit(revision);
  fixture.emit(revision);
  fixture.reconnect();
  await flush();
  fixture.emit(revision);
  await flush();
  assert.equal(pending.mock.callCount(), 1);
  assert.equal(full.mock.callCount(), 1);
});

for (const [error, retryable] of [
  [new ApiError('catching up', 'pending_projection_pending', 503, 1000), true],
  [new ApiError('busy', 'resource_overloaded', 503, 1000), true],
  [new ApiError('denied', 'forbidden', 403), false],
  [new ApiError('document preparing', 'doc_range_index_pending', 503, 1000), false],
  [new ApiError('long wait', 'pending_projection_pending', 503, 10001), false],
  [new ApiTransportError('disconnected'), false],
] as const) {
  test(`unchanged notices recover only retryable failed pending queries (${error.message})`, async context => {
    const revision = { graph: '1', pending: '1' };
    const fixture = setup(context, revision);
    fixture.client.setQueryData(pendingKey, durable('old', revision));
    await assert.rejects(fixture.client.fetchQuery({ queryKey: pendingKey, retry: false, queryFn: async () => { throw error; } }));
    context.mock.method(api, 'repositoryRevision', async () => revision);
    const pending = context.mock.method(api, 'pendingState', async () => durable('recovered', revision));
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('old', revision)));
    fixture.start();
    await flush();
    assert.equal(pending.mock.callCount(), retryable ? 1 : 0);
    assert.equal(fixture.client.getQueryState(pendingKey)?.status, retryable ? 'success' : 'error');
    fixture.emit(revision);
    fixture.reconnect();
    await flush();
    assert.equal(pending.mock.callCount(), retryable ? 1 : 0);
    assert.equal(full.mock.callCount(), 0);
  });
}
