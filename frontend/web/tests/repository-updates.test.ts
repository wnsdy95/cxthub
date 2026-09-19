import assert from 'node:assert/strict';
import {mergePendingView,pendingViewNeedsFull,parseRevision,revisionCovers,subscribeRepository} from '../src/repositoryUpdates';
import {validateGraphState} from '../src/graphState';
import {serverGraphFixture} from './serverGraphFixture';
import type {Snapshot,Pending,PendingView,RepositoryView} from '../src/types';
const snapshot=(id:string,parents:string[]=[]):Snapshot=>({id,parents,repo_id:'repo',doc_hash:id,provider:'codex',branch:'main',created_at:'2026-09-01T00:00:00Z',message:id==='committed'?'commit':'hook: capture'});
const pending=(target:string):Pending=>({target,session_id:'live',repo_id:'repo',branch:'main',provider:'codex',updated_at:'2026-09-01T00:00:00Z'});
const refs=[{kind:'branch' as const,name:'main',repo_id:'repo',target:'committed'}];
const full=(snapshots:Snapshot[],target:string,seq='2')=>serverGraphFixture({refs,snapshots,pending:[pending(target)],revision:{graph:'2',pending:seq}});
const patch=(v:RepositoryView):PendingView=>({revision:v.revision!,graph:v.graph,pending:v.pending,snapshots:v.snapshots.filter(s=>v.pending.some(p=>p.target===s.id)).map(({branches:_ignored,...s})=>s)});
const view=full([snapshot('committed'),snapshot('old',['committed'])],'old','1');
const update=patch(full([snapshot('committed'),snapshot('new',['committed'])],'new'));
assert.deepEqual(mergePendingView(view,update).snapshots.map(s=>s.id),['committed','new']);
assert.equal(mergePendingView(view,update).graph,update.graph);
assert.equal(mergePendingView(view,{...update,revision:{graph:'3',pending:'2'}}),view);
assert.equal(mergePendingView({...view,revision:{graph:'2',pending:'3'}},update).revision?.pending,'3');
const retained=full([snapshot('committed'),snapshot('old'),snapshot('child',['old']),snapshot('new')],'new');
assert.deepEqual(mergePendingView({...view,snapshots:[...view.snapshots,snapshot('child',['old'])]},patch(retained)).snapshots.map(s=>s.id),['committed','old','child','new']);
assert.equal(revisionCovers({graph:'9007199254740993',pending:'2'},{graph:'9007199254740992',pending:'2'}),true);
assert.equal(parseRevision({graph:'1',pending:'bad'}),null);
assert.equal(pendingViewNeedsFull(view,update),false);
for(const graft of [false,true]) {
 const current=full([snapshot('committed'),{...snapshot('new',graft?[]:['unseen']),...(graft?{graft_parents:['unseen']}:{})}],'new');
 assert.equal(pendingViewNeedsFull(view,patch(current)),true);
 const known={...view,snapshots:[...view.snapshots,...current.snapshots]};
 assert.equal(pendingViewNeedsFull(known,patch(current)),false);
}
const staged=patch(full([snapshot('committed'),snapshot('new'),snapshot('staged')],'new'));
assert.equal(pendingViewNeedsFull(view,staged),true,'metadata staged at the same graph revision still needs one complete read');
assert.throws(()=>validateGraphState(undefined,view.revision),/Unsupported/);
assert.throws(()=>mergePendingView(view,{...update,graph:{...update.graph,revision:{graph:'2',pending:'9'}}}),/Inconsistent/);
const enriched={...view,snapshots:[{...snapshot('old'),branches:['main','topic'],memory_hash:'memory-1'}]};
const raw=patch(full([{...snapshot('old'),memory_hash:'memory-2'}],'old'));
const refreshed=mergePendingView(enriched,raw).snapshots[0];
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
