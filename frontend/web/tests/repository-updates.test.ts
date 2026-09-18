import assert from 'node:assert/strict';
import {mergePendingView,pendingViewNeedsFull,parseRevision,revisionCovers,subscribeRepository} from '../src/repositoryUpdates';
import type {RepositoryView,Snapshot,Pending} from '../src/types';
const snapshot = (id: string, parents: string[] = []): Snapshot => ({id,parents} as Snapshot);
const view: RepositoryView = {revision:{graph:'2',pending:'1'}, refs:[],history:[],reflog:[],unsync:[],pending:[{target:'old'} as Pending],snapshots:[snapshot('committed'),snapshot('old')]};
const update = {revision:{graph:'2',pending:'2'},pending:[{target:'new'} as Pending],snapshots:[snapshot('new')]};
assert.deepEqual(mergePendingView(view,update).snapshots.map(s=>s.id),['committed','new']);
assert.equal(mergePendingView(view,{...update,revision:{graph:'3',pending:'2'}}),view);
assert.equal(mergePendingView({...view,revision:{graph:'2',pending:'3'}},update).revision?.pending,'3');
assert.deepEqual(mergePendingView({...view,snapshots:[...view.snapshots,snapshot('child',['old'])]},update).snapshots.map(s=>s.id),['committed','old','child','new']);
assert.equal(revisionCovers({graph:'9007199254740993',pending:'2'},{graph:'9007199254740992',pending:'2'}),true);
assert.equal(parseRevision({graph:'1',pending:'bad'}),null);
assert.equal(pendingViewNeedsFull(view,update),false);
assert.equal(pendingViewNeedsFull(view,{...update,snapshots:[snapshot('new',['unseen'])]}),true);
assert.equal(pendingViewNeedsFull(view,{...update,snapshots:[{...snapshot('new'),graft_parents:['unseen']}]}),true);
assert.equal(pendingViewNeedsFull(view,{...update,snapshots:[snapshot('new',['old'])]}),false);
assert.deepEqual(mergePendingView(view,{...update,snapshots:[snapshot('new',['old'])]}).snapshots.map(s=>s.id),['committed','old','new']);
const knownBroken = {...view,snapshots:[...view.snapshots,snapshot('new',['unseen'])]};
assert.equal(pendingViewNeedsFull(knownBroken,{...update,snapshots:[snapshot('new',['unseen'])]}),false); // full view already exposes this diagnostic

// /view enriches membership; /pending-view only owns capture metadata.
const enriched = {...view, snapshots:[{...snapshot('old'), branches:['main','topic'], memory_hash:'memory-1'}]};
const raw = {revision:{graph:'2',pending:'2'}, pending:[{target:'old'} as Pending],
  snapshots:[{...snapshot('old'), memory_hash:'memory-2'}]};
const refreshed = mergePendingView(enriched,raw).snapshots[0];
assert.deepEqual(refreshed.branches,['main','topic']);
assert.equal(refreshed.memory_hash,'memory-2');

// Multiple consumers share a stream; malformed notifications must recover
// rather than silently leaving the browser's view stale.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  listener?: (event: {data: string}) => void;
  onerror?: () => void;
  closed = false;
  constructor() { FakeEventSource.instances.push(this); }
  addEventListener(_type: string, listener: (event: {data: string}) => void) { this.listener = listener; }
  close() { this.closed = true; }
}
const original = globalThis.EventSource;
Object.assign(globalThis, {EventSource: FakeEventSource});
try {
  let changes = 0, failures = 0;
  const listener = () => ({changed: () => changes++, failed: () => failures++});
  const releaseA = subscribeRepository('/synthetic/repo/changes', listener());
  const releaseB = subscribeRepository('/synthetic/repo/changes', listener());
  assert.equal(FakeEventSource.instances.length, 1);
  const stream = FakeEventSource.instances[0];
  stream.listener?.({data: '{"graph":"1","pending":"2"}'});
  assert.equal(changes, 2);
  stream.listener?.({data: '{"graph":"bad","pending":"2"}'});
  stream.listener?.({data: 'broken JSON'});
  assert.equal(failures, 4);
  releaseA();
  assert.equal(stream.closed, false);
  releaseB();
  assert.equal(stream.closed, true);
} finally { Object.assign(globalThis, {EventSource: original}); }
