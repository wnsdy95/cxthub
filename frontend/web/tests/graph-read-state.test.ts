import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError, ApiTransportError } from '../src/api';
import { decodeGraphState, type GraphWire } from '../src/graphWire';
import { graphPositionMatches, graphReadState, graphRefreshTarget, parseGraphProjection, parseGraphReadGeneration, validateGraphPosition, validateGraphRead } from '../src/graphReadState';
import { retryGraphRead } from '../src/useRepositoryUpdates';
import { mergePendingView, parseRevision, pendingViewNeedsFull } from '../src/repositoryUpdates';
import type { GraphProjectionStatus, GraphReadGeneration } from '../src/types';
import { fullFixture, pendingFixture, wireFixture } from './pendingViewFixture';

const incarnation = '12345678-abcd-4321-9876-123456789abc';
const generation: GraphReadGeneration = { generation: `sha256:${'a'.repeat(64)}`, source_epoch: `${incarnation}:2`, applied_epoch: `${incarnation}:1`, rules: 2, complete: true, current: false };
const status: GraphProjectionStatus = { ...generation, state: 'catching_up' };
const view = () => {
  const pending = pendingFixture();
  return fullFixture({ ...pending, read_generation: generation, graph: { ...pending.graph, read_generation: generation } });
};

test('graph notices validate independently of pending and retain stable complete generation across writes', () => {
  assert.equal(parseGraphProjection(status), status);
  const next = { ...status, source_epoch: `${incarnation}:9223372036854775807` };
  assert.equal(parseGraphProjection(next), next);
  assert.equal(graphRefreshTarget(status), graphRefreshTarget(next));
  assert.ok(parseRevision({ graph: '1', pending: '2', graph_projection: status }));
  for (const state of ['disabled', 'needs_attention', 'catching_up', 'ready'] as const) {
    const notice = { ...status, source_epoch: generation.applied_epoch, state, current: true };
    assert.equal(parseGraphProjection(notice), notice);
  }
  const reset = { state: 'catching_up' as const, source_epoch: `22345678-abcd-4321-9876-123456789abc:0`, applied_epoch: generation.applied_epoch, rules: 2 as const, complete: false, current: false };
  assert.equal(parseGraphProjection(reset), reset);
  for (const corrupt of [
    null, [], {}, { ...status, rules: 1 }, { ...status, rules: 3 }, { ...status, current: true },
    { ...status, complete: false }, { ...status, generation: undefined }, { ...status, applied_epoch: undefined },
    { ...status, source_epoch: `${incarnation}:9223372036854775808` }, { ...status, source_epoch: `${incarnation}:02` },
    { ...status, applied_epoch: `${incarnation}:3` }, { ...status, state: 'ready' },
    { ...status, source_epoch: status.source_epoch + '\n' }, { ...status, generation: status.generation + '\n' },
    { ...status, source_epoch: reset.source_epoch },
  ]) assert.equal(parseRevision({ graph: '1', pending: '2', graph_projection: corrupt }), null, JSON.stringify(corrupt));
});

test('bodies must be complete, internally coherent and compatible with the applied source, not the latest head', () => {
  const body = view();
  assert.equal(validateGraphRead(body, { ...status, source_epoch: `${incarnation}:99` }), body);
  const newer = { ...status, generation: `sha256:${'b'.repeat(64)}`, applied_epoch: `${incarnation}:2`, source_epoch: `${incarnation}:3` };
  assert.equal(validateGraphRead(body, newer), body);
  for (const changes of [{ rules: 1 }, { complete: false }, { current: true }, { applied_epoch: `${incarnation}:3` },
    { source_epoch: `${incarnation}:01` }, { generation: 'bad' }]) {
    assert.equal(parseGraphReadGeneration({ ...generation, ...changes }), null);
  }
  for (const changes of [{ read_generation: undefined }, { graph: { ...body.graph, read_generation: undefined } },
    { graph: { ...body.graph, read_generation: { ...generation, generation: newer.generation! } } },
    { graph: { ...body.graph, read_generation: { ...generation, source_epoch: `${incarnation}:3` } } }]) {
    assert.throws(() => validateGraphRead({ ...body, ...changes }, status));
  }
  for (const invalid of [{ ...status, applied_epoch: `${incarnation}:0` },
    { ...status, source_epoch: `${incarnation}:0`, applied_epoch: `${incarnation}:0`, current: true },
    { ...newer, source_epoch: `22345678-abcd-4321-9876-123456789abc:3` },
    { ...status, generation: newer.generation }]) assert.throws(() => validateGraphRead(body, invalid));
  assert.throws(() => validateGraphRead(fullFixture(), status));
  assert.doesNotThrow(() => validateGraphRead(fullFixture()));
});

test('legacy and unmatched pending patches cannot replace durable graph metadata', () => {
  const body = view(), legacy = pendingFixture();
  assert.equal(pendingViewNeedsFull(body, legacy), true);
  assert.equal(mergePendingView(body, legacy), body);
  const mismatch = { ...legacy, read_generation: { ...generation, generation: `sha256:${'b'.repeat(64)}` }, graph: body.graph };
  assert.equal(pendingViewNeedsFull(body, mismatch), true);
  assert.equal(mergePendingView(body, mismatch), body);
  assert.equal(pendingViewNeedsFull(fullFixture(), body), true);
  assert.deepEqual(mergePendingView(body, body).read_generation, generation);
});

test('irrelevant compact patches are discarded before payload validation; accepted patches remain strict', () => {
  const full = fullFixture();
  const differentGraph = {...pendingFixture(), revision: {graph: '2', pending: '2'}};
  assert.equal(pendingViewNeedsFull(full, differentGraph), true);
  assert.equal(mergePendingView(full, differentGraph), full);
  const stale = {...pendingFixture(), revision: {graph: '1', pending: '1'}};
  assert.equal(mergePendingView(full, stale), full);
  const inconsistent = {...pendingFixture(), graph: {...full.graph, revision: {graph: '1', pending: '9'}}};
  assert.throws(() => pendingViewNeedsFull(full, inconsistent), /Inconsistent/);
  assert.throws(() => mergePendingView(full, inconsistent), /Inconsistent/);
});

test('V1/V2 decoding and full/live API normalization preserve graph read metadata', async context => {
  const body = view();
  for (const encoding of ['indexed-v1', 'indexed-v2'] as const) {
    const wire = { ...wireFixture(body), graph: { ...wireFixture(body).graph, encoding } };
    assert.deepEqual(decodeGraphState(wire.graph as GraphWire).read_generation, generation);
    context.mock.method(globalThis, 'fetch', async () => new Response(JSON.stringify(wire), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    for (const read of [api.repositoryView, api.pendingView]) {
      const normalized = await read('repo');
      assert.deepEqual(normalized.read_generation, generation);
      assert.deepEqual(normalized.graph.read_generation, generation);
    }
    context.mock.restoreAll();
  }
});

test('UI keeps lagged complete graphs visible but hides resets and denied views', () => {
  assert.deepEqual(graphReadState(view(), status), { state: 'lagging', showGraph: true });
  assert.deepEqual(graphReadState(view(), { ...status, state: 'needs_attention' }), { state: 'failed', showGraph: true });
  assert.deepEqual(graphReadState(view(), { state: 'catching_up', source_epoch: `22345678-abcd-4321-9876-123456789abc:0`, rules: 2, complete: false, current: false }), { state: 'pending', showGraph: false });
  assert.deepEqual(graphReadState(view(), status, undefined, new ApiError('denied', 'forbidden', 403)), { state: 'denied', showGraph: false });
  assert.deepEqual(graphReadState(view(), { ...status, state: 'ready', source_epoch: generation.applied_epoch, current: true }), { state: 'ready', showGraph: true });
});

test('body metadata labels initial lag without notices; only explicit disabled status uses legacy display', () => {
  const lagged = view();
  assert.deepEqual(graphReadState(lagged), { state: 'lagging', showGraph: true });
  const current = { ...generation, source_epoch: generation.applied_epoch, current: true };
  assert.deepEqual(graphReadState({ ...lagged, read_generation: current, graph: { ...lagged.graph, read_generation: current } }), { state: 'ready', showGraph: true });
  assert.deepEqual(graphReadState(lagged, { ...status, state: 'disabled' }), { state: 'legacy', showGraph: true });
  assert.deepEqual(graphReadState(fullFixture()), { state: 'legacy', showGraph: true });
  assert.deepEqual(graphReadState({ ...lagged, read_generation: undefined }), { state: 'pending', showGraph: false });
  assert.deepEqual(graphReadState({ ...lagged, graph: { ...lagged.graph, read_generation: undefined } }), { state: 'pending', showGraph: false });
  assert.deepEqual(graphReadState(lagged, undefined, new ApiError('terminal', 'graph_projection_failed', 503)), { state: 'failed', showGraph: true });
});

test('graph retries are bounded and exclusive to graph preparation or shared capacity errors', () => {
  const pending = new ApiError('preparing', 'graph_projection_pending', 503, 1000);
  assert.equal(retryGraphRead(0, pending), true);
  assert.equal(retryGraphRead(8, pending), true);
  assert.equal(retryGraphRead(9, pending), false);
  for (const error of [new ApiError('failed', 'graph_projection_failed', 503), new ApiError('denied', 'forbidden', 403),
    new ApiError('other projection', 'pending_projection_pending', 503), new ApiError('long wait', 'graph_projection_pending', 503, 10001), new ApiTransportError('offline')]) {
    assert.equal(retryGraphRead(0, error), false);
  }
  assert.equal(retryGraphRead(0, new ApiError('busy', 'resource_overloaded', 503, 1000)), true);
});

test('historical graphs require the selected complete generation even when all legacy counters match', () => {
  const full = view();
  assert.equal(graphPositionMatches(full.graph, full.revision, generation), true);
  assert.equal(graphPositionMatches({ ...full.graph, read_generation: { ...generation, source_epoch: `${incarnation}:99` } }, full.revision, generation), true);
  const legacy = fullFixture();
  assert.equal(graphPositionMatches(legacy.graph, legacy.revision), true);
  assert.equal(graphPositionMatches(legacy.graph, legacy.revision, generation), false);
  assert.equal(graphPositionMatches(full.graph, full.revision), false);
  assert.equal(graphPositionMatches(full.graph, { ...full.revision, graph: '9' }, generation), false);
  for (const change of [
    { generation: `sha256:${'b'.repeat(64)}` }, { applied_epoch: `${incarnation}:0` },
    { source_epoch: '22345678-abcd-4321-9876-123456789abc:2', applied_epoch: '22345678-abcd-4321-9876-123456789abc:1' },
  ]) assert.equal(graphPositionMatches({ ...full.graph, read_generation: { ...generation, ...change } }, full.revision, generation), false);
  for (const change of [{ rules: 1 }, { complete: false }, { current: true }, { generation: 'invalid' }]) {
    const graph = { ...full.graph, read_generation: { ...generation, ...change } } as typeof full.graph;
    assert.throws(() => validateGraphPosition(graph));
    assert.equal(graphPositionMatches(graph, full.revision, generation), false);
  }
  assert.throws(() => validateGraphPosition({ ...full.graph, version: 2 }));
});
