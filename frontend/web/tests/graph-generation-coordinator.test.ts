import assert from 'node:assert/strict';
import { test, type TestContext } from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../src/api';
import { graphReadState } from '../src/graphReadState';
import { readRepositoryGraph, startRepositoryUpdates } from '../src/useRepositoryUpdates';
import type { RepositoryRevision, RepositoryView } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture } from './pendingViewFixture';

const fullKey = ['repo-view', 'repo'], noticeKey = ['repository-notice', 'repo'];
const incarnation = '12345678-abcd-4321-9876-123456789abc';
const flush = () => new Promise<void>(resolve => setImmediate(resolve));
function notice(applied = 1, source = applied, digest = 'a', epoch = incarnation): RepositoryRevision {
  return { graph: '1', pending: '1', evidence: '1', graph_projection: {
    state: source === applied ? 'ready' : 'catching_up', source_epoch: `${epoch}:${source}`, applied_epoch: `${epoch}:${applied}`,
    rules: 2, generation: `sha256:${digest.repeat(64)}`, complete: true, current: source === applied,
  } };
}
function body(revision: RepositoryRevision): RepositoryView {
  const status = revision.graph_projection!;
  const generation = { generation: status.generation!, source_epoch: status.source_epoch, applied_epoch: status.applied_epoch!, rules: 2 as const, complete: true as const, current: status.current };
  const full = fullFixture(pendingFixture(status.generation, revision));
  return { ...full, read_generation: generation, graph: { ...full.graph, read_generation: generation } };
}
function deferred<Value>() {
  let resolve!: (value: Value) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<Value>((accept, fail) => { resolve = accept; reject = fail; });
  return { promise, resolve, reject };
}
function setup(context: TestContext, initial = notice()) {
  context.mock.timers.enable({ apis: ['setTimeout'] });
  context.mock.method(Math, 'random', () => 0);
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, 'document'), originalSource = Object.getOwnPropertyDescriptor(globalThis, 'EventSource');
  const document = Object.assign(new EventTarget(), { hidden: false });
  const streams = new Set<EventTarget>();
  class Source extends EventTarget {
    constructor() { super(); streams.add(this); }
    close() { streams.delete(this); }
  }
  Object.assign(globalThis, { document, EventSource: Source });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  client.setQueryData(fullKey, body(initial));
  client.setQueryData(['pending-state', 'repo', 'on-hold-v1'], pendingStateFixture(pendingFixture('cached', initial)));
  const pending = context.mock.method(api, 'pendingView', async () => pendingFixture());
  context.mock.method(api, 'pendingState', async () => pendingStateFixture(pendingFixture('live', initial)));
  let stop: (() => void) | undefined;
  context.after(() => {
    stop?.(); client.clear();
    for (const [key, descriptor] of [['document', originalDocument], ['EventSource', originalSource]] as const) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else Reflect.deleteProperty(globalThis, key);
    }
  });
  return { client, pending, start: () => { stop = startRepositoryUpdates('repo', client); }, stop: () => stop?.(),
    hide: () => { document.hidden = true; document.dispatchEvent(new Event('visibilitychange')); },
    reconnect: () => document.dispatchEvent(new Event('visibilitychange')),
    emit: (next: RepositoryRevision) => { for (const stream of streams) stream.dispatchEvent(new MessageEvent('revision', { data: JSON.stringify(next) })); },
  };
}

test('continuous head and pending changes never refetch the same applied graph; worker publication with unchanged counters does', async context => {
  const initial = notice(), fixture = setup(context, initial);
  let latest = initial;
  context.mock.method(api, 'repositoryRevision', async () => latest);
  const full = context.mock.method(api, 'repositoryView', async () => body(latest));
  fixture.start(); await flush();
  for (let sequence = 2; sequence < 100; sequence++) {
    latest = { ...notice(1, sequence), graph: String(sequence), pending: String(sequence), pending_projection: {
      state: 'catching_up', rules: 1, source_epoch: `${incarnation}:${sequence}`,
    } };
    fixture.emit(latest); await flush();
  }
  assert.equal(full.mock.callCount(), 0);
  assert.equal(fixture.pending.mock.callCount(), 0);
  assert.deepEqual(graphReadState(fixture.client.getQueryData(fullKey), latest.graph_projection), { state: 'lagging', showGraph: true });
  latest = { ...latest, graph_projection: notice(99, 99, 'b').graph_projection };
  fixture.emit(latest); await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, latest.graph_projection?.generation);
  fixture.emit(latest); fixture.reconnect(); await flush();
  assert.equal(full.mock.callCount(), 1);
});

test('a head advance during a full read accepts the prior complete generation without a synchronous loop', async context => {
  const fixture = setup(context), published = notice(2, 2, 'b'), response = deferred<RepositoryView>();
  context.mock.method(api, 'repositoryRevision', async () => published);
  const full = context.mock.method(api, 'repositoryView', () => response.promise);
  fixture.start(); await flush();
  for (let sequence = 3; sequence < 60; sequence++) fixture.emit(notice(2, sequence, 'b'));
  response.resolve(body(published)); await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, published.graph_projection?.generation);
  assert.equal(fixture.client.getQueryState(fullKey)?.status, 'success');
});

test('an in-flight old complete response drains only a newly published generation', async context => {
  const fixture = setup(context), first = notice(2, 2, 'b'), latest = notice(3, 3, 'c'), response = deferred<RepositoryView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0 ? response.promise : Promise.resolve(body(latest)));
  fixture.start(); await flush(); fixture.emit(latest);
  response.resolve(body(first)); await flush();
  assert.equal(full.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, latest.graph_projection?.generation);
});

test('source incarnation reset cancels old responses and waits for a new complete generation', async context => {
  const fixture = setup(context), first = notice(2, 2, 'b'), response = deferred<RepositoryView>();
  const reset = notice(0, 0, 'c', '22345678-abcd-4321-9876-123456789abc');
  const waiting: RepositoryRevision = { ...reset, graph_projection: { state: 'catching_up', source_epoch: reset.graph_projection!.source_epoch, rules: 2, complete: false, current: false } };
  context.mock.method(api, 'repositoryRevision', async () => first);
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0 ? response.promise : Promise.resolve(body(reset)));
  fixture.start(); await flush(); fixture.emit(waiting); await flush();
  response.resolve(body(first)); await flush();
  assert.equal(fixture.client.getQueryState(fullKey)?.fetchStatus, 'idle');
  assert.equal(full.mock.callCount(), 1);
  assert.deepEqual(graphReadState(fixture.client.getQueryData(fullKey), waiting.graph_projection), { state: 'pending', showGraph: false });
  fixture.emit(reset); await flush();
  assert.equal(full.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, reset.graph_projection?.generation);
});

test('terminal graph failures do not retry automatically on head, reconnect or duplicate notices', async context => {
  const fixture = setup(context), published = notice(2, 2, 'b');
  context.mock.method(api, 'repositoryRevision', async () => published);
  const full = context.mock.method(api, 'repositoryView', async () => { throw new ApiError('failed', 'graph_projection_failed', 503); });
  fixture.start(); await flush();
  for (let source = 3; source < 9; source++) { fixture.emit(notice(2, source, 'b')); fixture.reconnect(); context.mock.timers.tick(120000); await flush(); }
  assert.equal(full.mock.callCount(), 1);
  fixture.emit(notice(3, 3, 'c')); await flush();
  assert.equal(full.mock.callCount(), 2);
});

for (const status of [401, 403]) test(`unchanged-generation revision ${status} cancels late bodies and forces fresh authorization recovery`, async context => {
  const fixture = setup(context), published = notice(2, 2, 'b'), response = deferred<RepositoryView>();
  let denied = false;
  context.mock.method(api, 'repositoryRevision', async () => {
    if (denied) throw new ApiError('denied', 'forbidden', status);
    return published;
  });
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0 ? response.promise : Promise.resolve(body(published)));
  fixture.start(); await flush(); denied = true; fixture.reconnect(); await flush();
  response.resolve(body(published)); await flush(); fixture.emit(published); await flush();
  assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'error');
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, notice().graph_projection?.generation);
  assert.equal(full.mock.callCount(), 1);
  denied = false; fixture.reconnect(); await flush();
  assert.equal(full.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryState(noticeKey)?.status, 'success');
});

for (const lifecycle of ['hide', 'stop'] as const) test(`${lifecycle} suppresses generation followups`, async context => {
  const fixture = setup(context), first = notice(2, 2, 'b'), response = deferred<RepositoryView>();
  context.mock.method(api, 'repositoryRevision', async () => first);
  const full = context.mock.method(api, 'repositoryView', () => response.promise);
  fixture.start(); await flush(); fixture.emit(notice(3, 3, 'c')); fixture[lifecycle]();
  response.resolve(body(first)); await flush(); context.mock.timers.tick(120000); await flush();
  assert.equal(full.mock.callCount(), 1);
});

test('full-read validation consults only its repository query key and rejects future applied generations', async context => {
  const fixture = setup(context), current = notice();
  fixture.client.setQueryData(noticeKey, current);
  fixture.client.setQueryData(['repository-notice', 'other'], notice(1, 1, 'b', '22345678-abcd-4321-9876-123456789abc'));
  context.mock.method(api, 'repositoryView', async () => body(current));
  assert.deepEqual(await readRepositoryGraph('repo', fixture.client), body(current));
  context.mock.method(api, 'repositoryView', async () => body(notice(2, 2, 'b')));
  await assert.rejects(readRepositoryGraph('repo', fixture.client), /published source/);
});

test('incomplete notices wait without graph bodies, then fetch a complete lagged bundle', async context => {
  const fixture = setup(context), published = notice(2, 3, 'b');
  const waiting: RepositoryRevision = { ...published, graph_projection: {
    state: 'catching_up', source_epoch: published.graph_projection!.source_epoch, rules: 2, complete: false, current: false,
  } };
  context.mock.method(api, 'repositoryRevision', async () => waiting);
  const full = context.mock.method(api, 'repositoryView', async () => body(published));
  fixture.start(); await flush(); fixture.emit(waiting); fixture.reconnect(); await flush();
  assert.equal(full.mock.callCount(), 0);
  fixture.emit(published); await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(fixture.pending.mock.callCount(), 0);
  assert.equal(graphReadState(fixture.client.getQueryData(fullKey), published.graph_projection).state, 'lagging');
});

test('needs-attention notices cancel graph preparation retries but preserve completed cached rows', async context => {
  const fixture = setup(context), published = notice(2, 2, 'b');
  context.mock.method(api, 'repositoryRevision', async () => published);
  const full = context.mock.method(api, 'repositoryView', async () => { throw new ApiError('preparing', 'graph_projection_pending', 503, 1000); });
  fixture.start(); await flush();
  const failed: RepositoryRevision = { ...published, graph_projection: { ...published.graph_projection!, state: 'needs_attention' } };
  fixture.emit(failed); await flush();
  context.mock.timers.tick(10000); await flush();
  assert.equal(full.mock.callCount(), 1);
  assert.equal(fixture.client.getQueryState(fullKey)?.fetchStatus, 'idle');
  assert.deepEqual(graphReadState(fixture.client.getQueryData(fullKey), failed.graph_projection), { state: 'failed', showGraph: true });
});

test('observer-owned full reads coalesce a newer generation into one validated followup', async context => {
  const fixture = setup(context), first = notice(), latest = notice(2, 2, 'b'), response = deferred<RepositoryView>();
  const full = context.mock.method(api, 'repositoryView', () => full.mock.callCount() === 0 ? response.promise : Promise.resolve(body(latest)));
  const observer = fixture.client.fetchQuery({ queryKey: fullKey, queryFn: ({ signal }) => readRepositoryGraph('repo', fixture.client, signal) });
  context.mock.method(api, 'repositoryRevision', async () => latest);
  fixture.start(); await flush(); response.resolve(body(first)); await observer; await flush();
  assert.equal(full.mock.callCount(), 2);
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.read_generation?.generation, latest.graph_projection?.generation);
});

test('malformed graph versions cannot enter the cache even with matching notice metadata', async context => {
  const fixture = setup(context), published = notice(2, 2, 'b');
  context.mock.method(api, 'repositoryRevision', async () => published);
  context.mock.method(api, 'repositoryView', async () => {
    const valid = body(published);
    return { ...valid, graph: { ...valid.graph, version: 2 } } as unknown as RepositoryView;
  });
  fixture.start(); await flush();
  assert.equal(fixture.client.getQueryState(fullKey)?.status, 'error');
  assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.graph.version, 1);
});

for (const projection of [undefined, { ...notice().graph_projection!, state: 'disabled' as const }]) {
  test(`legacy compact refresh is preserved with ${projection ? 'disabled' : 'absent'} graph notices`, async context => {
    const fixture = setup(context), previous = { graph: '1', pending: '1' }, latest = { graph: '1', pending: '2', graph_projection: projection };
    fixture.client.setQueryData(fullKey, fullFixture(pendingFixture('old', previous)));
    context.mock.method(api, 'repositoryRevision', async () => latest);
    const full = context.mock.method(api, 'repositoryView', async () => fullFixture(pendingFixture('full', latest)));
    context.mock.method(api, 'pendingView', async () => pendingFixture('live', latest));
    fixture.start(); await flush();
    assert.equal(full.mock.callCount(), 0);
    assert.equal(fixture.client.getQueryData<RepositoryView>(fullKey)?.pending[0].target, 'live');
  });
}
