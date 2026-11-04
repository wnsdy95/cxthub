import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, ApiError } from '../src/api';
import { pendingStateEpochChanged, pendingStateRows, validatePendingState } from '../src/pendingState';
import type { PendingStateView } from '../src/types';
import { fullFixture, pendingFixture, pendingStateFixture, wireFixture } from './pendingViewFixture';

const sourceIncarnation = '12345678-abcd-4321-9876-123456789abc';
const readGeneration: NonNullable<PendingStateView['read_generation']> = {
  generation: `sha256:${'a'.repeat(64)}`,
  source_epoch: `${sourceIncarnation}:42`,
  applied_epoch: `${sourceIncarnation}:42`,
  rules: 1,
  complete: true,
};

test('optional read generations accept complete matching canonical epochs without losing decimal64 precision', () => {
  const legacy = pendingStateFixture();
  assert.equal(validatePendingState(legacy), legacy);
  for (const sequence of ['0', '1', '9007199254740993', '9223372036854775807']) {
    const epoch = `${sourceIncarnation}:${sequence}`;
    const view = { ...legacy, read_generation: { ...readGeneration, source_epoch: epoch, applied_epoch: epoch } };
    const before = JSON.stringify(view);
    assert.equal(validatePendingState(view), view);
    const rows = pendingStateRows(undefined, view);
    assert.equal(rows.ready, true);
    assert.deepEqual(rows.orphans.map(item => item.session_id), ['live-session']);
    assert.equal('graph' in view, false);
    assert.equal(JSON.stringify(view), before);
  }
});

test('read generations reject missing, mistyped, incomplete and unsupported publication metadata', () => {
  for (const generation of [null, false, 1, 'ready', [], {}]) {
    assert.throws(() => validatePendingState({ ...pendingStateFixture(), read_generation: generation } as PendingStateView), /Invalid On Hold read generation/);
  }
  const invalidFields: Record<string, unknown[]> = {
    rules: [undefined, null, 0, 2, '1', true],
    complete: [undefined, null, false, 1, 'true'],
    generation: [undefined, null, 42, '', 'a'.repeat(64), `sha256:${'a'.repeat(63)}`, `sha256:${'a'.repeat(65)}`,
      `sha256:${'A'.repeat(64)}`, `sha256:${'g'.repeat(64)}`, `SHA256:${'a'.repeat(64)}`, ` ${readGeneration.generation}`],
    source_epoch: [undefined, null, 42],
    applied_epoch: [undefined, null, 42],
  };
  for (const [field, values] of Object.entries(invalidFields)) for (const value of values) {
    const view = { ...pendingStateFixture(), read_generation: { ...readGeneration, [field]: value } } as PendingStateView;
    assert.throws(() => validatePendingState(view), /Invalid On Hold read generation/, `${field}=${JSON.stringify(value)}`);
  }
});

test('read generations reject source/applied mismatch even beyond the safe integer range', () => {
  for (const [source, applied] of [
    [`${sourceIncarnation}:42`, `${sourceIncarnation}:41`],
    [`${sourceIncarnation}:42`, `${sourceIncarnation}:43`],
    [`${sourceIncarnation}:9007199254740992`, `${sourceIncarnation}:9007199254740993`],
    [`${sourceIncarnation}:42`, '22345678-abcd-4321-9876-123456789abc:42'],
  ]) {
    const view = { ...pendingStateFixture(), read_generation: { ...readGeneration, source_epoch: source, applied_epoch: applied } };
    assert.throws(() => validatePendingState(view), /Invalid On Hold read generation/, `${source} != ${applied}`);
  }
});

test('read generations reject noncanonical UUIDs and decimal64 epochs even when source and applied match', () => {
  const epochs = [
    '', sourceIncarnation, `${sourceIncarnation.toUpperCase()}:42`, `${sourceIncarnation.replaceAll('-', '')}:42`,
    `{${sourceIncarnation}}:42`, `urn:uuid:${sourceIncarnation}:42`, '12345678-abcd-4321-9876-123456789abg:42',
    '12345678-abcd-4321-9876-123456789ab:42', ` ${sourceIncarnation}:42`, `${sourceIncarnation}:42:42`,
    ...['', '-1', '+1', '00', '01', '1.0', '1e2', ' 1', '1 ', '１', '9223372036854775808', '18446744073709551615', '9'.repeat(100)]
      .map(sequence => `${sourceIncarnation}:${sequence}`),
  ];
  for (const epoch of epochs) {
    const view = { ...pendingStateFixture(), read_generation: { ...readGeneration, source_epoch: epoch, applied_epoch: epoch } };
    assert.throws(() => validatePendingState(view), /Invalid On Hold read generation/, JSON.stringify(epoch));
  }
});

test('read generations reject trailing line terminators in digest and matching epochs', () => {
  for (const suffix of ['\n', '\r', '\u2028', '\u2029']) {
    const epoch = readGeneration.source_epoch + suffix;
    for (const generation of [
      { ...readGeneration, generation: readGeneration.generation + suffix },
      { ...readGeneration, source_epoch: epoch, applied_epoch: epoch },
    ]) {
      assert.throws(() => validatePendingState({ ...pendingStateFixture(), read_generation: generation }),
        /Invalid On Hold read generation/, JSON.stringify(generation));
    }
  }
});

test('pending API validates read generations before returning a successful response without transport retries', async () => {
  const previousFetch = globalThis.fetch;
  let requests = 0;
  let generation: unknown = readGeneration;
  globalThis.fetch = async () => {
    requests++;
    return new Response(JSON.stringify({ ...pendingStateFixture(), read_generation: generation }));
  };
  try {
    assert.deepEqual((await api.pendingState('repo')).read_generation, readGeneration);
    assert.equal(requests, 1);
    for (const invalid of [null, { ...readGeneration, rules: 2 }, { ...readGeneration, complete: false },
      { ...readGeneration, applied_epoch: `${sourceIncarnation}:41` }]) {
      generation = invalid;
      const before = requests;
      await assert.rejects(() => api.pendingState('repo'), /Invalid On Hold read generation/);
      assert.equal(requests, before + 1);
    }
  } finally { globalThis.fetch = previousFetch; }
});

test('compact pending results produce session rows before any full graph is available', () => {
  const pending = pendingStateFixture();
  const rows = pendingStateRows(undefined, pending);
  assert.deepEqual(rows.orphans.map(item => item.session_id), ['live-session']);
  assert.equal(rows.archiveSnapshots[0].doc_hash, 'pending-document');
  assert.equal(rows.ready, true);
  assert.equal(pendingStateRows().ready, false);
  assert.equal('graph' in rows, false);
});

test('durable pending is authoritative independently of full graph revision order', () => {
  for (const revision of [
    { graph: '0', pending: '0', evidence: '0' },
    { graph: '1', pending: '2', evidence: '0' },
    { graph: '100', pending: '100', evidence: '100' },
    { graph: '0', pending: '100', evidence: '1' },
  ]) {
    const full = fullFixture(pendingFixture('full', revision));
    const durable = { ...pendingStateFixture(pendingFixture('durable')), read_generation: readGeneration };
    assert.equal(pendingStateRows(full, durable).pendings[0].target, 'durable');
    const empty = { ...durable, pending: [], snapshots: [], orphan_sessions: [], hold_counts: {} };
    assert.deepEqual(pendingStateRows(full, empty).pendings, []);
    assert.equal(pendingStateRows(full, empty).ready, true);
  }
});

test('durable epoch resets compare only published UUIDs, not counters or full graph revisions', () => {
  const previous = { ...pendingStateFixture(), read_generation: readGeneration };
  const sequence = `${sourceIncarnation}:43`;
  const later = { ...previous, read_generation: { ...readGeneration, source_epoch: sequence, applied_epoch: sequence } };
  assert.equal(pendingStateEpochChanged(previous, later), false);
  const epoch = '22345678-abcd-4321-9876-123456789abc:0';
  const reset = { ...previous, read_generation: { ...readGeneration, source_epoch: epoch, applied_epoch: epoch } };
  assert.equal(pendingStateEpochChanged(previous, reset), true);
  assert.equal(pendingStateEpochChanged(previous, { ...previous, revision: { graph: '0', pending: '0' } }), false);
  assert.equal(pendingStateEpochChanged(undefined, reset), false);
  assert.equal(pendingStateEpochChanged(pendingStateFixture(), reset), false);
  assert.equal(pendingStateEpochChanged(previous, pendingStateFixture()), false);
});

test('cached pending rows remain visible but not ready during refresh or explicit projection lag', () => {
  const durable = { ...pendingStateFixture(), read_generation: readGeneration };
  const lag = new ApiError('On Hold projection is catching up', 'pending_projection_pending', 503, 1000);
  for (const read of [{ fetching: true }, { fetching: true, failureReason: lag }, { error: lag }]) {
    const rows = pendingStateRows(fullFixture(), durable, read);
    assert.equal(rows.ready, false);
    assert.equal(rows.refreshing, true);
    assert.equal(rows.inaccessible, false);
    assert.equal(rows.pendings[0].target, 'pending-document');
  }
  assert.equal(pendingStateRows(fullFixture(), undefined, { failureReason: lag }).ready, false);
  const empty = { ...durable, pending: [], snapshots: [], orphan_sessions: [] };
  assert.equal(pendingStateRows(undefined, empty, { failureReason: lag }).ready, false);
  assert.equal(pendingStateRows(undefined, durable).ready, true);
  assert.equal(pendingStateRows(undefined, durable).refreshing, false);
});

test('cheap projection notices suppress cached live rows before any fetch starts', () => {
  const pending = { ...pendingStateFixture(), read_generation: readGeneration };
  for (const state of ['catching_up', 'needs_attention'] as const) {
    const projection = { state, source_epoch: readGeneration.source_epoch, rules: 1 as const };
    const rows = pendingStateRows(fullFixture(), pending, { projection });
    assert.equal(rows.ready, false);
    assert.equal(rows.refreshing, state === 'catching_up');
    assert.equal(rows.needsAttention, state === 'needs_attention');
    assert.equal(rows.inaccessible, false);
    assert.equal(rows.pendings[0].target, 'pending-document');
    assert.equal(pendingStateRows(undefined, undefined, { projection }).ready, false);
    assert.equal(pendingStateRows(fullFixture(), undefined, { projection }).ready, false);
  }
});

test('ready notices expose cached rows as live only for the exact published generation and epoch', () => {
  const pending = { ...pendingStateFixture(), read_generation: readGeneration };
  const projection = { state: 'ready' as const, source_epoch: readGeneration.source_epoch, rules: 1 as const, generation: readGeneration.generation };
  assert.equal(pendingStateRows(undefined, pending, { projection }).ready, true);
  for (const status of [
    { ...projection, generation: `sha256:${'b'.repeat(64)}` },
    { ...projection, source_epoch: `${sourceIncarnation}:43` },
    { ...projection, source_epoch: '22345678-abcd-4321-9876-123456789abc:42' },
  ]) {
    const rows = pendingStateRows(fullFixture(), pending, { projection: status });
    assert.equal(rows.ready, false);
    assert.equal(rows.refreshing, true);
  }
  assert.equal(pendingStateRows(fullFixture(), pendingStateFixture(), { projection }).ready, false);
  assert.equal(pendingStateRows(fullFixture(), undefined, { projection }).ready, false);
  assert.equal(pendingStateRows(undefined, pending, { projection: { state: 'disabled', source_epoch: projection.source_epoch, rules: 1 } }).ready, true);
  assert.equal(pendingStateRows(undefined, pending).ready, true);
  const denied = new ApiError('Access denied', 'forbidden', 403);
  const rows = pendingStateRows(fullFixture(), pending, { projection, error: denied });
  assert.equal(rows.inaccessible, true);
  assert.equal(rows.refreshing, false);
  assert.deepEqual(rows.pendings, []);
});

test('known pending-incarnation resets hide cached rows and legacy full fallback, unlike same-incarnation lag', () => {
  const pending = { ...pendingStateFixture(), read_generation: readGeneration };
  const full = fullFixture(pendingFixture('obsolete-full'));
  const resetEpoch = '22345678-abcd-4321-9876-123456789abc:0';
  for (const state of ['catching_up', 'needs_attention', 'disabled', 'ready'] as const) {
    const projection = { state, source_epoch: resetEpoch, rules: 1 as const, ...(state === 'ready' ? { generation: readGeneration.generation } : {}) };
    const rows = pendingStateRows(full, pending, { projection: projection as import('../src/types').PendingProjectionStatus });
    assert.equal(rows.ready, false);
    assert.equal(rows.sourceReset, true);
    assert.deepEqual(rows.pendings, []);
    assert.deepEqual(rows.archiveSnapshots, []);
    assert.deepEqual(rows.chains, []);
    assert.deepEqual(rows.refs, []);
  }
  const lagged = pendingStateRows(full, pending, { projection: { state: 'catching_up', source_epoch: `${sourceIncarnation}:99`, rules: 1 } });
  assert.equal(lagged.ready, false);
  assert.equal(lagged.sourceReset, false);
  assert.equal(lagged.pendings[0].target, 'pending-document');
  assert.equal(pendingStateRows(undefined, pending).sourceReset, false);
  assert.equal(pendingStateRows(undefined, pending).pendings[0].target, 'pending-document');
});

test('fresh independent pending data survives an older full generation without borrowing its metadata', () => {
  const pending = { ...pendingStateFixture(pendingFixture('fresh')), read_generation: readGeneration };
  const full = { ...fullFixture(pendingFixture('old')), read_generation: {
    ...readGeneration, rules: 2 as const, current: true, source_epoch: '22345678-abcd-4321-9876-123456789abc:1', applied_epoch: '22345678-abcd-4321-9876-123456789abc:1',
  } };
  const rows = pendingStateRows(full, pending, { projection: { state: 'ready', source_epoch: readGeneration.source_epoch, generation: readGeneration.generation, rules: 1 } });
  assert.equal(rows.ready, true);
  assert.equal(rows.sourceReset, false);
  assert.equal(rows.pendings[0].target, 'fresh');
  assert.equal(rows.archiveSnapshots[0].id, 'fresh');
});

test('access errors from either read hide every cached pending row and its metadata', () => {
  const durable = { ...pendingStateFixture(), read_generation: readGeneration };
  for (const status of [401, 403, 404]) for (const field of ['error', 'failureReason', 'fullError', 'noticeError'] as const) {
    const denied = new ApiError('Access denied', 'forbidden', status);
    const rows = pendingStateRows(fullFixture(), durable, { [field]: denied, fetching: true });
    assert.equal(rows.inaccessible, true);
    assert.equal(rows.ready, false);
    assert.equal(rows.refreshing, false);
    assert.equal(rows.error, 'Access denied');
    for (const list of [rows.refs, rows.archiveSnapshots, rows.pendings, rows.orphans, rows.chains]) assert.deepEqual(list, []);
    assert.equal(rows.archiveBySnapshot.size, 0);
  }
});

test('late full graphs and stale compact views cannot regress pending generations', () => {
  const old = fullFixture(pendingFixture('old', { graph: '1', pending: '1' }));
  const current = pendingStateFixture(pendingFixture('new', { graph: '1', pending: '2' }));
  const view = pendingStateRows(old, current);
  assert.equal(view.pendings[0].target, 'new');
  assert.deepEqual(view.archiveSnapshots.map(snapshot => snapshot.id), ['new']);
  assert.equal(old.pending[0].target, 'old');
  assert.equal(pendingStateRows(fullFixture(pendingFixture('new')), pendingStateFixture(pendingFixture('old', { graph: '1', pending: '1' }))).pendings[0].target, 'new');
  for (const revision of [{ graph: '0', pending: '9' }, { graph: '1', pending: '2', evidence: '0' }]) {
    const full = fullFixture(pendingFixture('evidence', { graph: '1', pending: '2', evidence: '3' }));
    assert.equal(pendingStateRows(full, pendingStateFixture(pendingFixture('stale', revision))).pendings[0].target, 'evidence');
  }
});

test('list metadata never borrows snapshots or refs across graph generations', () => {
  const old = fullFixture();
  old.refs = [{ kind: 'branch', name: 'main', target: 'pending-document', repo_id: 'repo' }];
  const next = pendingFixture('new', { graph: '2', pending: '3' });
  const view = pendingStateRows(old, pendingStateFixture(next));
  assert.deepEqual(view.archiveSnapshots.map(snapshot => snapshot.id), ['new']);
  assert.deepEqual(view.refs, []);
  const archived = pendingFixture();
  archived.graph.orphan_sessions = [];
  assert.deepEqual(pendingStateRows(undefined, pendingStateFixture(archived)).orphans, []);
});

test('lite projection and complete graph APIs reject the other contract', async () => {
  const previousFetch = globalThis.fetch;
  const full = fullFixture();
  const lite = pendingStateFixture();
  let response: unknown = lite;
  const urls: string[] = [];
  globalThis.fetch = async url => { urls.push(String(url)); return new Response(JSON.stringify(response)); };
  try {
    assert.deepEqual(await api.pendingState('repo'), lite);
    assert.ok(urls.at(-1)!.endsWith('/pending-view?projection=on-hold-v1'));
    await assert.rejects(() => api.pendingView('repo'), /graph encoding/);
    response = wireFixture(full);
    assert.deepEqual((await api.pendingView('repo')).graph.operations, full.graph.operations);
    await assert.rejects(() => api.pendingState('repo'), /On Hold projection/);
    assert.throws(() => validatePendingState({ ...lite, graph: full.graph } as typeof lite), /On Hold projection/);
    const before = JSON.stringify(full);
    pendingStateRows(full, pendingStateFixture(pendingFixture('fresh', { graph: '2', pending: '3' })));
    assert.equal(JSON.stringify(full), before);
  } finally { globalThis.fetch = previousFetch; }
});
