import assert from 'node:assert/strict';
import {serverGraphFixture} from './serverGraphFixture';
import {projectBranchGraph, visibleBranchGraph} from '../src/graphProjection';
import {GraphIndex} from '../src/graphIndex';
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
