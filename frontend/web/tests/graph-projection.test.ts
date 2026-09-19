import assert from 'node:assert/strict';
import { projectBranchGraph } from './serverGraphFixture';
import { layoutGraph } from '../src/graph.ts';
import { GraphIndex } from '../src/graphIndex.ts';
import type { Snapshot, HistoryEvent, RefLogEntry, Ref } from '../src/types.ts';

const at=(n:number)=>`2026-09-16T00:00:0${n}Z`;
const snap=(id:string,branch:string,parents:string[],n:number,graft:string[]=[]):Snapshot=>({id,branch,parents,graft_parents:graft,grafted:graft.length>0,repo_id:'repo',doc_hash:id,created_at:at(n),provider:'codex',fidelity:'full',message:id});
const snapshots=[snap('root','main',[],0),snap('base','main',['root'],2),snap('feature','feature',['root'],3,['base']),snap('tip','feature',['feature'],4)];
const refs:Ref[]=[{repo_id:'repo',kind:'branch',name:'main',target:'tip'},{repo_id:'repo',kind:'branch',name:'feature',target:'tip'}];
const birth:HistoryEvent={repo_id:'repo',id:'birth',branch_id:'feature-id',kind:'birth',branch:'feature',source:'root',target:'root',created_at:at(1)};
const advance:HistoryEvent={...birth,id:'advance',kind:'advance',source:'root',target:'feature',created_at:at(3)};
const log:RefLogEntry={kind:'branch',name:'main',old:'base',new:'tip',created_at:at(5)};
const original=JSON.stringify(snapshots);

// A valid same-branch Join can supersede a historical append edge while
// preserving all content. Completion is immutable; current placement is not.
{
  const before = [snap('P','main',[],0), snap('X','main',['P'],1),
    snap('H','main',['P'],2,['X']), snap('H2','main',['H'],3)];
  const after = [before[0], {...before[1], graft_parents:['H2'], grafted:true},
    {...before[2], graft_parents:[], grafted:false}, before[3]];
  const receipt: HistoryEvent = {...birth, id:'reordered-completion', kind:'pr-merge',
    branch:'main', branch_id:'main-id', source_branch_id:'topic-id',
    source:'H', target:'H', shared_target:'X', pr_completed:true,
    pr:{number:42,base_branch:'main',head_branch:'topic',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)}};
  for (const [data,head] of [[before,'H2'],[after,'X']] as const) {
    const result = projectBranchGraph(data,[{repo_id:'repo',kind:'branch',name:'main',branch_id:'main-id',target:head}],[receipt],[],head,'main');
    assert.ok(result.events.has('graph:merge:reordered-completion'), 'a later reorder must not erase the completed operation');
    assert.deepEqual(new GraphIndex(result.snapshots).issues, [], 'retained operations must not recreate an old placement cycle');
  }
}
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
assert.equal(row('feature').snap.grafted,false,'represented overlay must not turn the natural birth edge into a legacy graft');
assert.equal(JSON.stringify(snapshots),original,'projection cannot rewrite archive parents');

const same=projectBranchGraph([snapshots[0]], [{repo_id:'repo',kind:'branch',name:'feature',target:'root'}], [birth], [],'root','main');
assert.equal(same.snapshots.length,2,'same-hash branch birth must remain a separate event');
const pending:HistoryEvent={...birth,id:'receipt',kind:'pr-merge',branch_id:'main-id',branch:'main',source:'tip',target:'tip',shared_target:'base',source_branch_id:'feature-id',pr:{number:1,base_branch:'main',head_branch:'feature',head_sha:'a'.repeat(40),merge_sha:'b'.repeat(40)},created_at:at(5)};
assert.equal([...projectBranchGraph(snapshots,refs,[pending],[],'tip','main').events.values()].filter(e=>e.kind==='merge').length,0,'pending receipt is not a completed merge');
assert.equal([...projectBranchGraph(snapshots,refs,[],[{...log,old:'tip',new:'root'}],'root','main').events.values()].length,0,'rewind is not a join');
console.log('graph projection tests passed');

// Later refs alone cannot turn an ordinary main fast-forward into a merge.
const ordinary=[snap('root','main',[],0),snap('tip','main',['root'],1)];
assert.equal([...projectBranchGraph(ordinary,refs,[],[{...log,old:'root',new:'tip'}],'tip','main').events.values()].filter(e=>e.kind==='merge').length,0);
// Name reuse creates two identities, not one overwrite; a bound receipt selects the correct birth.
const reused={...birth,id:'reused-birth',branch_id:'reused',binding_parent:'rename',created_at:at(6)};
const renamed={...birth,id:'rename',kind:'rename' as const,branch:'renamed',previous_branch:'feature',binding_parent:birth.id,created_at:at(4)};
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

// A server completion proves the no-op join even without any ref movement.
const done:HistoryEvent={...pending,id:'done',pr_completed:true,source:'root',target:'root',shared_target:'root'};
const sameRefs=refs.map(r=>({...r,target:'root'}));
const complete=projectBranchGraph([snapshots[0]],sameRefs,[birth,done],[],'root','main');
const cm=[...complete.events].find(([,e])=>e.kind==='merge')![0];
assert.deepEqual(complete.snapshots.find(s=>s.id===cm)?.parents,['root','graph:birth:birth']);
assert.equal(complete.pinHead,cm);
const cr=layoutGraph(complete.snapshots,complete.pinHead).rows;
assert.equal(cr.find(r=>r.snap.id===cm)?.lane,0);
assert.notEqual(cr.find(r=>r.snap.id==='graph:birth:birth')?.lane,0);
// One successful append with both a ref movement and completion draws one join.
const appended={...pending,id:'appended',pr_completed:true,created_at:at(6)};
assert.equal([...projectBranchGraph(snapshots,refs,[birth,advance,pending,appended],[log],'tip','main').events.values()].filter(e=>e.kind==='merge').length,1);
// Already-contained source may differ from the unchanged main tip.
const contained={...done,source:'root',target:'tip',shared_target:'tip'};
const containedProjection=projectBranchGraph(ordinary,refs,[birth,contained],[],'tip','main');
const containedMerge=[...containedProjection.events].find(([,e])=>e.kind==='merge')![0];
assert.deepEqual(containedProjection.snapshots.find(s=>s.id===containedMerge)?.parents,['tip','graph:birth:birth']);


// Ordinary automatic capture finalizes a publication without a rewind/advance.
// Two archived branches fork at one baseline and merge between main captures.
const autoSnapshots = [
  snap('root', 'main', [], 0), snap('base', 'main', ['root'], 1),
  snap('left', 'feature/left', ['root'], 3, ['base']),
  snap('middle', 'main', ['left'], 5),
  snap('right', 'feature/right', ['root'], 6, ['middle']),
  snap('current', 'main', ['right'], 8),
];
const autoHistory: HistoryEvent[] = [];
for (const [name, before, n] of [['left', 'base', 3], ['right', 'middle', 6]] as const) {
  const b: HistoryEvent = {...birth, id: `${name}-birth`, branch_id: `${name}-id`, branch: `feature/${name}`, created_at: at(n-1)};
  autoHistory.push(b,
    {...b, id: `${name}-position`, kind: 'position', source: name, target: name, created_at: at(n)},
    {...b, id: `${name}-publish`, kind: 'publish', source: name, target: name, created_at: at(n)},
    {...b, id: `${name}-merge`, kind: 'pr-merge', branch_id: 'main-id', branch: 'main', source_branch_id: b.branch_id,
      source: name, target: name, shared_target: before, pr_completed: true,
      pr: {...pending.pr!, head_branch: b.branch}, created_at: at(n+1)},
    {...b, id: `${name}-archive`, kind: 'archive', binding_parent: b.id, source: name, target: name, created_at: at(9)});
}
const autoRefs: Ref[] = [{repo_id:'repo', kind:'branch', name:'main', branch_id:'main-id', target:'current'}];
const beforeAuto = JSON.stringify([autoSnapshots, autoHistory, autoRefs]);
for (const history of [autoHistory, [...autoHistory].reverse(), autoHistory.filter(e => e.kind !== 'publish')]) {
  const projected = projectBranchGraph(autoSnapshots, autoRefs, history, [], 'current', 'main');
  const byId = new Map(projected.snapshots.map(s => [s.id, s]));
  for (const [name, previous] of [['left', 'base'], ['right', 'middle']]) {
    assert.deepEqual(byId.get(name)?.parents, [`graph:birth:${name}-birth`], 'finalized source must reach its own birth');
    assert.deepEqual(byId.get(`graph:birth:${name}-birth`)?.parents, ['root']);
    assert.deepEqual(byId.get(`graph:merge:${name}-merge`)?.parents, [previous, name]);
  }
  assert.deepEqual(byId.get('middle')?.parents, ['graph:merge:left-merge']);
  assert.deepEqual(byId.get('current')?.parents, ['graph:merge:right-merge']);
  const layout = layoutGraph(projected.snapshots, projected.pinHead).rows;
  for (const name of ['left', 'right']) {
    const born = layout.find(r => r.snap.id === `graph:birth:${name}-birth`)!;
    const source = layout.find(r => r.snap.id === name)!;
    assert.equal(source.outgoing[source.lane], born.snap.id);
    assert.equal(born.incoming[source.lane], born.snap.id);
    assert.equal(born.lane, source.lane);
    assert.notEqual(source.lane, 0);
  }
}

assert.equal(JSON.stringify([autoSnapshots, autoHistory, autoRefs]), beforeAuto);
// Viewing somebody else's snapshot is not proof of branch ownership.
const onlySelected = autoHistory.filter(e => e.kind === 'birth' || e.kind === 'position');
assert.deepEqual(projectBranchGraph(autoSnapshots, autoRefs, onlySelected, []).snapshots.find(s => s.id === 'left')?.parents, ['root']);
// A reused name without the original identity cannot take its publication.
const otherBirth = {...birth, id:'other-birth', branch_id:'other-id', branch:'feature/left', binding_parent:'left-archive'};
const reusedAuto = projectBranchGraph(autoSnapshots, autoRefs, [...autoHistory, otherBirth], []);
assert.deepEqual(reusedAuto.snapshots.find(s => s.id === 'left')?.parents, ['graph:birth:left-birth']);
// Publication also connects a root-only orphan after its ref is archived.
const orphanPublished = {...oa, kind:'publish' as const, source:'orphan-tip'};
const publishedOrphan = projectBranchGraph([snap('root','main',[],0),snap('orphan-tip','feature',[],4)], [], [orphan, orphanPublished], []);
assert.deepEqual(publishedOrphan.snapshots.find(s=>s.id==='orphan-tip')?.parents, ['graph:birth:orphan']);

// Two identities can publish the same content; neither owns its edge exclusively.
const competing = {...autoHistory.find(e=>e.id==='left-publish')!, id:'other-publication', branch_id:otherBirth.branch_id};
const competingHistory = [...autoHistory, otherBirth, competing];
for (const history of [competingHistory, [...competingHistory].reverse()]) {
  const ambiguous = projectBranchGraph(autoSnapshots, autoRefs, history, []);
  assert.deepEqual(ambiguous.snapshots.find(s=>s.id==='left')?.parents, ['root']);
}
// A completed source with no stored path to this birth cannot invent one.
const unrelated = autoSnapshots.map(s=>s.id==='left' ? {...s, parents:[]} : s);
assert.deepEqual(projectBranchGraph(unrelated, autoRefs, autoHistory, []).snapshots.find(s=>s.id==='left')?.parents, []);
// Publishing from an old path selected on an orphan cannot claim that old root.
const selectedOrphan = projectBranchGraph([snap('root','main',[],0), snap('orphan-tip','feature',['root'],4)],
  [], [orphan, {...orphan, id:'selection', kind:'position', target:'root'}, orphanPublished], []);
assert.deepEqual(selectedOrphan.snapshots.find(s=>s.id==='root')?.parents, []);
assert.deepEqual(selectedOrphan.snapshots.find(s=>s.id==='orphan-tip')?.parents, ['root']);

// Repeated completions at one stored main tip form a continuous operation
// chain. A subsequent main capture must not bypass the second and third PR.
const repeatedHistory: HistoryEvent[] = [];
for (let n = 1; n <= 3; n++) {
  const b = {...birth, id:`repeat-birth-${n}`, branch_id:`repeat-${n}`, branch:`feature/${n}`};
  repeatedHistory.push(b, {...done, id:`repeat-merge-${n}`, source_branch_id:b.branch_id,
    pr:{...done.pr!, number:n, head_branch:b.branch}, created_at:at(n+1)});
}
for (const history of [repeatedHistory, [...repeatedHistory].reverse()]) {
  const repeat = projectBranchGraph([snap('root','main',[],0), snap('current','main',['root'],6)],
    [{repo_id:'repo',kind:'branch',name:'main',target:'current'}], history, [], 'current','main');
  const map = new Map(repeat.snapshots.map(s=>[s.id,s]));
  assert.equal(map.get('current')!.parents![0], 'graph:merge:repeat-merge-3');
  assert.equal(map.get('graph:merge:repeat-merge-3')!.parents![0], 'graph:merge:repeat-merge-2');
  assert.equal(map.get('graph:merge:repeat-merge-2')!.parents![0], 'graph:merge:repeat-merge-1');
  const layout = layoutGraph(repeat.snapshots,repeat.pinHead).rows;
  for (const row of layout.filter(r=>r.snap.id.startsWith('graph:merge:'))) {
    assert.equal(row.lane,0);
    assert.equal(row.incoming[0], row.snap.id, 'every completed PR stays attached to main');
  }
}

// Provider timestamps cannot assign retained content to a particular PR.
// Without a recorded continuation, keep the stored historical edge unchanged.
const historical = projectBranchGraph([snap('root','main',[],0), snap('between','main',['root'],2.5), snap('current','main',['root'],6)],
  [{repo_id:'repo',kind:'branch',name:'main',target:'current'}], repeatedHistory, [], 'current','main');
assert.deepEqual(historical.snapshots.find(s=>s.id==='between')?.parents, ['root']);
// Legacy destructive grafts retain an append seam in parents[0]; it cannot
// establish that the incoming conversation began at this birth.
const legacyBirth = projectBranchGraph([snap('root','main',[],0), {...snap('legacy','feature',['root'],3), grafted:true}],
  [], [birth, {...advance, target:'legacy'}], []);
assert.deepEqual(legacyBirth.snapshots.find(s=>s.id==='legacy')?.parents, ['root']);

// An ordinary feature publication with a graft must not invent feature <- main
// merely because main later points to the same capture after its real PR.
const ownPublish = [snap('root','main',[],0), snap('source','feature',['root'],3,['root'])];
const ownRefs: Ref[] = [{repo_id:'repo',kind:'branch',name:'main',target:'source'}, {repo_id:'repo',kind:'branch',name:'feature',target:'source'}];
const ownDone = {...done,id:'own-done',source:'source',target:'source',shared_target:'root',created_at:at(5)};
const ownMove = {...log,name:'feature',old:'root',new:'source',created_at:at(4)};
const ownGraph = projectBranchGraph(ownPublish,ownRefs,[birth,ownDone],[ownMove],'source','main');
assert.deepEqual([...ownGraph.events.values()].filter(e=>e.kind==='merge').map(e=>[e.branch,e.sourceBranch]), [['main','feature']]);
const deduplicated = ownPublish.map(s=>({...s,branch:'main'}));
const published = {...birth,id:'own-publication',kind:'publish' as const,target:'source',created_at:at(3)};
assert.deepEqual([...projectBranchGraph(deduplicated,ownRefs,[birth,published,ownDone],[ownMove],'source','main').events.values()]
  .filter(e=>e.kind==='merge').map(e=>[e.branch,e.sourceBranch]), [['main','feature']]);

// Same branch identity establishes an operation path, not missing conversation
// parents. Two PRs reusing older content still retain two distinct side lanes.
const oldContent = [snap('old','main',[],0), snap('base','main',['old'],1), snap('current','main',['base'],8)];
const operationHistory: HistoryEvent[] = [1,2].flatMap(n=> {
  const b = {...birth,id:`operation-birth-${n}`,branch_id:`operation-${n}`,branch:`feature/${n}`,source:'base',target:'base'};
  return [b,{...done,id:`operation-merge-${n}`,source_branch_id:b.branch_id,source:'old',target:'base',shared_target:'base',
    pr:{...done.pr!,head_branch:b.branch,number:n},created_at:at(n+3)}];
});
const originalOperations = JSON.stringify([oldContent,operationHistory]);
for (const history of [operationHistory,[...operationHistory].reverse()]) {
  const projected = projectBranchGraph(oldContent,[{...ownRefs[0],target:'current'}],history,[],'current','main');
  const map = new Map(projected.snapshots.map(s=>[s.id,s]));
  assert.equal(projected.lifecycleEdges.size,2);
  assert.deepEqual(map.get('old')?.parents,[]);
  assert.deepEqual(map.get('base')?.parents,['old']);
  const rows = layoutGraph(projected.snapshots,projected.pinHead).rows;
  for (const n of [1,2]) {
    const mid=`graph:merge:operation-merge-${n}`, bid=`graph:birth:operation-birth-${n}`;
    assert.deepEqual([...projected.lifecycleEdges.get(mid)!],[bid]);
    assert.deepEqual(map.get(mid)?.parents,[n===1?'base':'graph:merge:operation-merge-1',bid]);
    const merge = rows.find(r=>r.snap.id===mid)!;
    const birth = rows.find(r=>r.snap.id===bid)!;
    const lane = merge.branchesOut.find(l=>merge.outgoing[l]===bid)!;
    assert.equal(merge.lane,0);
    assert.ok(lane>0);
    assert.equal(birth.lane,lane);
    for (const row of rows.slice(rows.indexOf(merge)+1,rows.indexOf(birth))) {
      assert.equal(row.incoming[lane],bid);
      assert.equal(row.outgoing[lane],bid);
    }
  }
}
assert.equal(JSON.stringify([oldContent,operationHistory]),originalOperations);
// No operation connector may be invented from names, ambiguous births, future
// creation records or a pending request.
for (const history of [
  operationHistory.map(h=>h.kind==='pr-merge'?{...h,source_branch_id:undefined}:h),
  operationHistory.map(h=>h.kind==='birth'?{...h,created_at:at(9)}:h),
  operationHistory.map(h=>({...h,pr_completed:false})),
]) assert.equal(projectBranchGraph(oldContent,[],history,[]).lifecycleEdges.size,0);
const superseded = projectBranchGraph(oldContent,[],operationHistory.map(h=>h.kind==='pr-merge'?{...h,target:'old'}:h),[]);
assert.equal([...superseded.events.values()].filter(e=>e.kind==='merge').length,2,'changed placement cannot revoke server completion');
assert.deepEqual(new GraphIndex(superseded.snapshots).issues,[]);
assert.deepEqual(superseded.snapshots.find(s=>s.id==='current')?.parents,['base'],'historical operations must not take over current ancestry');
assert.equal(p.lifecycleEdges.size,0,'a natural source/birth path does not get a duplicate operation track');
assert.equal([...projectBranchGraph(snapshots,refs,[pending,{...pending,id:'ambiguous-receipt'}],[log]).events.values()]
  .filter(e=>e.kind==='merge').length,0,'ambiguous receipts cannot assign the first matching source identity');

assert.throws(()=>projectBranchGraph(oldContent,[],[...operationHistory,...operationHistory.filter(h=>h.kind==='birth').map(h=>({...h,id:`duplicate-${h.id}`}))],[]),/identity conflict/);
