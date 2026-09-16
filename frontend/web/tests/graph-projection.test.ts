import assert from 'node:assert/strict';
import { projectBranchGraph } from '../src/graphProjection.ts';
import { layoutGraph } from '../src/graph.ts';
import type { Snapshot, HistoryEvent, RefLogEntry, Ref } from '../src/types.ts';

const at=(n:number)=>`2026-09-16T00:00:0${n}Z`;
const snap=(id:string,branch:string,parents:string[],n:number,graft:string[]=[]):Snapshot=>({id,branch,parents,graft_parents:graft,grafted:graft.length>0,repo_id:'repo',doc_hash:id,created_at:at(n),provider:'codex',fidelity:'full',message:id});
const snapshots=[snap('root','main',[],0),snap('base','main',['root'],2),snap('feature','feature',['root'],3,['base']),snap('tip','feature',['feature'],4)];
const refs:Ref[]=[{repo_id:'repo',kind:'branch',name:'main',target:'tip'},{repo_id:'repo',kind:'branch',name:'feature',target:'tip'}];
const birth:HistoryEvent={repo_id:'repo',id:'birth',branch_id:'feature-id',kind:'birth',branch:'feature',source:'root',target:'root',created_at:at(1)};
const advance:HistoryEvent={...birth,id:'advance',kind:'advance',source:'root',target:'feature',created_at:at(3)};
const log:RefLogEntry={kind:'branch',name:'main',old:'base',new:'tip',created_at:at(5)};
const original=JSON.stringify(snapshots);
const p=projectBranchGraph(snapshots,refs,[birth,advance],[log],'tip','main');
const merge=[...p.events].find(([,e])=>e.kind==='merge')![0];
const born=[...p.events].find(([,e])=>e.kind==='birth')![0];
assert.equal(p.pinHead,merge);
const rows=layoutGraph(p.snapshots,p.pinHead).rows;
const row=(id:string)=>rows.find(r=>r.snap.id===id)!;
assert.equal(row(merge).lane,0);
assert.equal(row('base').lane,0,'main stays on the original main path');
assert.notEqual(row('tip').lane,0,'merged branch retains a side lane');
assert.equal(row('tip').lane,row('feature').lane);
assert.equal(row('feature').lane,row(born).lane,'branch line reaches its recorded birth');
assert.deepEqual(row(born).snap.parents,['root']);
assert.deepEqual(row(merge).snap.parents,['base','tip']);
assert.equal(JSON.stringify(snapshots),original,'projection cannot rewrite archive parents');

const same=projectBranchGraph([snapshots[0]], [{repo_id:'repo',kind:'branch',name:'feature',target:'root'}], [birth], [],'root','main');
assert.equal(same.snapshots.length,2,'same-hash branch birth must remain a separate event');
const pending:HistoryEvent={...birth,id:'receipt',kind:'pr-merge',branch:'main',source:'tip',target:'tip',shared_target:'base',source_branch_id:'feature-id',pr:{number:1,base_branch:'main',head_branch:'feature',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)},created_at:at(5)};
assert.equal([...projectBranchGraph(snapshots,refs,[pending],[],'tip','main').events.values()].filter(e=>e.kind==='merge').length,0,'pending receipt is not a completed merge');
assert.equal([...projectBranchGraph(snapshots,refs,[],[{...log,old:'tip',new:'root'}],'root','main').events.values()].length,0,'rewind is not a join');
console.log('graph projection tests passed');

// Later refs alone cannot turn an ordinary main fast-forward into a merge.
const ordinary=[snap('root','main',[],0),snap('tip','main',['root'],1)];
assert.equal([...projectBranchGraph(ordinary,refs,[],[{...log,old:'root',new:'tip'}],'tip','main').events.values()].filter(e=>e.kind==='merge').length,0);
// Name reuse creates two identities, not one overwrite; a bound receipt selects the correct birth.
const reused={...birth,id:'reused-birth',branch_id:'reused',created_at:at(6)};
const renamed={...birth,id:'rename',kind:'rename' as const,branch:'renamed',previous_branch:'feature',created_at:at(4)};
const q=projectBranchGraph(snapshots,refs,[birth,advance,reused,renamed,pending],[log],'tip','main');
assert.equal([...q.events.values()].filter(e=>e.kind==='birth').length,2);
assert.deepEqual(q.snapshots.find(s=>s.id==='feature')?.parents,['graph:birth:birth']);
// RFC3339 fractional seconds must compare by time, not raw string ordering.
const fraction={...pending,created_at:'2026-09-16T00:00:05.001Z'};
const ff=[snap('root','main',[],0),snap('base','main',['root'],2),snap('tip','feature',['base'],4)];
assert.equal([...projectBranchGraph(ff,refs,[fraction],[{...log,created_at:'2026-09-16T00:00:05Z'}],'tip','main').events.values()].filter(e=>e.kind==='merge').length,0);
// Root-only orphan starts retain memory provenance without conversation ancestry.
const orphan={...birth,id:'orphan',kind:'orphan' as const,source:undefined,target:undefined,memory_source:'root'};
const oa={...advance,source:undefined,target:'orphan-tip'};
const op=projectBranchGraph([snap('root','main',[],0),snap('orphan-tip','feature',[],4)],[],[orphan,oa],[]);
assert.deepEqual(op.snapshots.find(s=>s.id==='graph:birth:orphan')?.parents,[]);
assert.deepEqual(op.snapshots.find(s=>s.id==='orphan-tip')?.parents,['graph:birth:orphan']);
// A second parent already carried by a lane still needs an explicit curve.
const connected=layoutGraph([snap('merge','main',['left','right'],3),snap('other','topic',['right'],4),snap('left','main',[],1),snap('right','topic',[],0)],'merge');
const mr=connected.rows.find(r=>r.snap.id==='merge')!;
assert.ok(mr.branchesOut.some(lane=>mr.outgoing[lane]==='right'));
