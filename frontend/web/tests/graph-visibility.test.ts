import assert from 'node:assert/strict';
import { GraphIndex } from '../src/graphIndex';
import { completedBranchEvidence } from '../src/graphEvidence';
import { projectBranchGraph, visibleBranchGraph } from '../src/graphProjection';
import { layoutGraph, sessionBoundaries, compactionBoundaries } from '../src/graph';
import type { Snapshot, HistoryEvent, Ref } from '../src/types';
const at=(n:number)=>new Date(Date.UTC(2026,8,18,0,0,n)).toISOString();
const s=(id:string,parents:string[],branch:string,n:number,graft_parents:string[]=[]):Snapshot=>({id,doc_hash:id,repo_id:'repo',parents,branch,created_at:at(n),graft_parents,grafted:graft_parents.length>0,provider:'codex',session_id:id,compaction_count:n});
const snapshots=[s('root',[],'main',0),s('before',['root'],'main',1),s('source',['root'],'topic',3,['before']),s('current',['source'],'main',5),s('archive',['root'],'deleted',6),s('retained',['root'],'main',7)];
const birth:HistoryEvent={repo_id:'repo',id:'birth',branch_id:'topic-id',branch:'topic',kind:'birth',source:'root',target:'root',created_at:at(2)};
const merge:HistoryEvent={...birth,id:'merge',branch:'main',branch_id:'main-id',kind:'pr-merge',source_branch_id:birth.branch_id,source:'source',target:'source',shared_target:'before',created_at:at(4),pr_completed:true,pr:{number:1,head_branch:'topic',base_branch:'main',head_sha:'a',merge_sha:'b'}};
const refs:Ref[]=[{repo_id:'repo',kind:'branch',branch_id:'main-id',name:'main',target:'current'}];
const history=[birth,merge];
const index=new GraphIndex(snapshots);
const evidence=completedBranchEvidence(snapshots,history,index);
const projection=projectBranchGraph(snapshots,refs,history,[],'current','main',index,evidence);
const original=JSON.stringify([snapshots,history,projection.snapshots]);
const beforeQueries={...index.stats};
const map=new Map(projection.snapshots.map(s=>[s.id,s]));
// Exhaust every visibility subset, beyond normal archive/progress controls.
// Every retained edge must remain the exact full-view edge; never bridge a
// hidden capture, recompute evidence, or lose virtual parents of visible rows.
for(let mask=0;mask<1<<snapshots.length;mask++) {
  const visible=new Set(snapshots.filter((_,n)=>mask&(1<<n)).map(s=>s.id));
  const view=visibleBranchGraph(projection,visible);
  assert.equal(view.events,projection.events);
  assert.equal(view.lifecycleEdges,projection.lifecycleEdges);
  assert.equal(layoutGraph(view.snapshots,view.pinHead).issues.length,0);
  const ids=new Set(view.snapshots.map(s=>s.id));
  for(const node of view.snapshots) {
    assert.equal(node,map.get(node.id));
    if(!projection.events.has(node.id)) assert.ok(visible.has(node.id));
    for(const p of [...(node.parents??[]),...(node.graft_parents??[])]) {
      if(projection.events.has(p)) assert.ok(ids.has(p),'visible children retain virtual operation parents');
      else if(map.has(p)&&!visible.has(p)) assert.ok(view.foldedParents.has(p),'folded endpoint is explicit, not missing data');
    }
  }
}
assert.deepEqual(index.stats,beforeQueries,'folding performs no ancestry query');
assert.equal(JSON.stringify([snapshots,history,projection.snapshots]),original);
assert.equal(evidence[0].merged,true);
assert.equal(evidence[0].lineage,'natural');
assert.ok(sessionBoundaries(snapshots).has('source'));
assert.ok(compactionBoundaries(snapshots).has('source'));
// Duplicate/cyclic hidden inputs block the full projection, even if a visible
// subset would otherwise look perfectly valid.
for(const bad of [[...snapshots,snapshots[4]],[...snapshots,s('loop',['loop'],'deleted',8)]]) {
  const full=projectBranchGraph(bad,refs,history,[]);
  assert.equal(visibleBranchGraph(full,new Set(['current','root'])).snapshots.length,0);
}
