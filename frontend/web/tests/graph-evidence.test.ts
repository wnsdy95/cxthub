import assert from 'node:assert/strict';
import { completedBranchEvidence } from '../src/graphEvidence';
import type { Snapshot, HistoryEvent } from '../src/types';

const snap = (id: string, parents: string[] = [], graft_parents: string[] = []): Snapshot => ({
  id, repo_id:'repo', branch:'main', parents, graft_parents, doc_hash:id, provider:'codex', created_at:'2026-09-01T00:00:00Z',
});
const birth: HistoryEvent = {id:'birth',repo_id:'repo',branch_id:'topic',branch:'topic',kind:'birth',source:'base',target:'base',created_at:'2026-09-01T00:00:00Z'};
const merge: HistoryEvent = {...birth,id:'merge',kind:'pr-merge',branch_id:'main',branch:'main',source_branch_id:'topic',source:'source',target:'main',shared_target:'main',pr_completed:true,
  pr:{number:1,base_branch:'main',head_branch:'topic',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)}};
const data = [snap('base'),snap('source',['base']),snap('main',['source'])];
const classify = (snapshots=data,history=[birth,merge]) => completedBranchEvidence(snapshots,history)[0];
assert.equal(classify().lineage,'natural');
assert.equal(classify().placementIntact,true);
assert.equal(classify(data,[birth,{...merge,source:'base'}]).lineage,'unchanged');
assert.equal(classify([snap('base'),snap('source',[],['base']),snap('main',['source'])]).lineage,'graft');
assert.equal(classify([snap('base'),{...snap('source',['base']),grafted:true},snap('main',['source'])]).lineage,'graft', 'legacy append is not natural conversation ancestry');
assert.equal(classify([snap('base'),snap('source',['missing'],['base']),snap('main',['source'])]).lineage,'missing', 'unavailable natural ancestry cannot prove append exclusivity');
assert.equal(classify([snap('base'),snap('source'),snap('main',['source'])]).lineage,'disconnected');
assert.equal(classify([snap('base'),snap('source',['missing']),snap('main',['source'])]).lineage,'missing');
assert.equal(classify(data,[merge]).lineage,'unknown');
assert.equal(classify(data,[birth,{...birth,id:'duplicate'},merge]).lineage,'unknown');
assert.equal(classify(data,[{...birth,kind:'orphan',source:undefined,target:undefined},merge]).lineage,'orphan');
assert.equal(classify(data.filter(s=>s.id!=='main')).placementIntact,false);
assert.equal(classify(data.filter(s=>s.id!=='source')).sourceAvailable,false);
assert.equal(classify([snap('base'),snap('source'),snap('main')]).placementIntact,false);
assert.equal(completedBranchEvidence(data,[birth,{...merge,pr_completed:false}]).length,0);
// A reused name never substitutes for the missing branch identity.
assert.equal(classify(data,[{...birth,branch_id:'other-generation'},merge]).lineage,'unknown');
