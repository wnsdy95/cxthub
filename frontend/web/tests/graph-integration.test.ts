import assert from 'node:assert/strict';
import {serverGraphFixture} from './serverGraphFixture';
import {projectBranchGraph, visibleBranchGraph} from '../src/graphProjection';
import {GraphIndex} from '../src/graphIndex';
import {layoutGraph} from '../src/graph';
import type {BranchContext, GraphState, HistoryEvent, Snapshot, Ref} from '../src/types';

const snap=(id:string,parents:string[]=[]):Snapshot=>({id,doc_hash:id,repo_id:'repo',branch:id,parents,provider:'codex',fidelity:'full',created_at:'2026-01-01T00:00:00Z'});
for (const head of ['continuation','A','base']) {
 const snapshots=[snap('base'),snap('A',['base']),snap('B',['base']),snap('continuation',['base'])];
 const refs:Ref[]=[{kind:'branch',name:'main',repo_id:'repo',branch_id:'main-id',target:head}];
 const history:HistoryEvent[]=['A','B'].map((id,i)=>({id:`done-${id}`,repo_id:'repo',branch:'main',branch_id:'main-id',source_branch_id:`topic-${id}`,kind:'pr-merge',source:id,target:id,shared_target:'base',pr_completed:true,
  pr:{number:i+1,base_branch:'main',head_branch:`topic-${id}`,head_sha:'a'.repeat(40),merge_sha:String(i+1).repeat(40)},created_at:`2026-01-0${3-i}T00:00:00Z`}));
 const inclusion:BranchContext={branch_id:'main-id',snapshot_id:head,code_commit:'2'.repeat(40),reason:'selected_code',roots:['A','B',head],snapshot_ids:[head,'B','A','base'],
  merges:history.map((h,i)=>({event_id:h.id,source:h.source!,merge_sha:h.pr!.merge_sha,pr_number:i+1,state:'included',reason:'verified_git_order',order:1-i}))};
 const view=serverGraphFixture({snapshots,refs,history,graph:{branch_contexts:{main:inclusion}} as GraphState},'','integrations');
 const result=projectBranchGraph(snapshots,refs,view.graph,head,'main');
 const index=new GraphIndex(result.snapshots);
 assert.deepEqual(index.issues,[],`integration at ${head} created a cycle`);
 assert.ok(index.reaches(result.pinHead!,'graph:merge:done-A'));
 assert.ok(index.reaches(result.pinHead!,'graph:merge:done-B'));
 assert.ok(index.reaches(result.pinHead!,'A'));
 assert.ok(index.reaches(result.pinHead!,'B'));
 assert.ok(index.reaches('graph:merge:done-B','graph:merge:done-A'),'Git order must win over receipt times');
 const folded=visibleBranchGraph(result,new Set([head,'B','base']));
 assert.ok(folded.snapshots.some(s => s.id === 'graph:merge:done-A'),'folding cannot erase integration evidence');
 assert.deepEqual(snapshots.find(s=>s.id==='continuation')?.parents,['base'],'projection mutated original parents');
}
console.log('verified graph integration tests passed');

// The destination's checkpoints stay between its PRs. The source's recorded
// birth remains on its actual parent branch, including nested feature work.
for (const destination of ['main','release']) for (const origin of ['destination','other-feature']) for (const continuation of ['A','base']) {
 const snapshots=[snap('base'),snap('A',['base']),snap('checkpoint-1',[continuation]),snap('checkpoint-2',['checkpoint-1']),snap('other',['base']),snap('B',[origin==='destination'?'checkpoint-2':'other']),snap('current',['B'])];
 const refs:Ref[]=[{kind:'branch',name:destination,repo_id:'repo',branch_id:'destination-id',target:'current'},{kind:'branch',name:'other-feature',repo_id:'repo',branch_id:'other-id',target:'other'}];
 const source=origin==='destination'?'checkpoint-2':'other';
 const birth:HistoryEvent={id:'birth-B',repo_id:'repo',kind:'birth',branch:'feature-B',branch_id:'topic-B',source,target:source,created_at:'2026-01-01T00:00:00Z'};
 const history:HistoryEvent[]=[birth,...['A','B'].map((id,i)=>({id:`done-${id}`,repo_id:'repo',branch:destination,branch_id:'destination-id',source_branch_id:`topic-${id}`,kind:'pr-merge' as const,source:id,target:id,shared_target:i?'checkpoint-2':'base',pr_completed:true,
  pr:{number:i+1,base_branch:destination,head_branch:`feature-${id}`,head_sha:'a'.repeat(40),merge_sha:String(i+1).repeat(40)},created_at:`2026-01-0${3-i}T00:00:00Z`}))];
 const context:BranchContext={branch_id:'destination-id',snapshot_id:'current',code_commit:'2'.repeat(40),reason:'selected_code',roots:['base','A','checkpoint-2','B','current'],snapshot_ids:snapshots.map(s=>s.id),merges:history.slice(1).map((h,i)=>({event_id:h.id,source:h.source!,before:h.shared_target!,merge_sha:h.pr!.merge_sha,pr_number:i+1,state:'included',reason:'verified_git_order',order:1-i}))};
 const original=JSON.stringify(snapshots);
 const view=serverGraphFixture({snapshots,refs,history,default_branch:destination,graph:{branch_contexts:{[destination]:context}} as GraphState},'','integrations');
 const result=projectBranchGraph(snapshots,refs,view.graph,'current',destination);
 const index=new GraphIndex(result.snapshots);
 assert.deepEqual(index.issues,[]);
 const layout=layoutGraph(result.snapshots,result.pinHead).rows;
 for(const id of ['current','graph:merge:done-B','checkpoint-2','checkpoint-1','graph:merge:done-A']) assert.equal(layout.find(r=>r.snap.id===id)?.lane,0,`${destination}/${origin}: ${id} left the destination spine`);
 const born=result.snapshots.find(s=>s.id==='graph:birth:birth-B')!;
 assert.deepEqual(born.parents,[source],'PR destination must not move the birth');
 assert.deepEqual(result.snapshots.find(s=>s.id==='B')?.parents,[born.id]);
 assert.notEqual(layout.find(r=>r.snap.id==='B')?.lane,0);
 if(origin==='other-feature') assert.notEqual(layout.find(r=>r.snap.id==='other')?.lane,0,'nested source moved onto destination');
 assert.equal(result.edgeBranches.get('graph:merge:done-B')?.get('checkpoint-2'),destination);
 assert.equal(result.edgeBranches.get('graph:merge:done-B')?.get('B'),'feature-B');
 assert.equal(JSON.stringify(snapshots),original);
}
