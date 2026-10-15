import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api } from '../src/api';
import { pendingStateRows, validatePendingState } from '../src/pendingState';
import { fullFixture, pendingFixture, pendingStateFixture, wireFixture } from './pendingViewFixture';

test('compact pending results produce session rows before any full graph is available', () => {
  const pending = pendingStateFixture();
  const rows = pendingStateRows(undefined, pending);
  assert.deepEqual(rows.orphans.map(item => item.session_id), ['live-session']);
  assert.equal(rows.archiveSnapshots[0].doc_hash, 'pending-document');
  assert.equal(rows.ready, true);
  assert.equal(pendingStateRows().ready, false);
  assert.equal('graph' in rows, false);
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
