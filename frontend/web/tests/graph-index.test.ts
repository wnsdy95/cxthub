import assert from 'node:assert/strict';
import { GraphIndex, reachesSnapshot } from '../src/graphIndex';
import { layoutGraph } from '../src/graph';
import type { Snapshot } from '../src/types';
const s = (id:string, parents:string[] = [], graft_parents:string[] = []): Snapshot => ({id,doc_hash:id,repo_id:'repo',parents,graft_parents,provider:'codex',created_at:'2026-09-18T00:00:00Z'});

for (const input of [
  [s('self',['self'])],
  [s('a',['b']),s('b',['a']),s('ancestor')],
  [s('a',['b']),s('b',[],['a'])],
]) {
  const index = new GraphIndex(input);
  assert.equal(index.issues[0].kind,'cycle');
  assert.ok(!index.issues[0].ids.includes('ancestor'),'cycle witness does not blame unrelated or blocked ancestors');
  assert.equal(layoutGraph(input).rows.length,0);
}
const duplicates = [s('a'), s('a',['missing']),s('b')];
assert.deepEqual(new GraphIndex(duplicates).issues,[{kind:'duplicate-id',ids:['a'],count:1}]);
assert.equal(layoutGraph(duplicates).rows.length,0);
const partial = new GraphIndex([s('a',['missing'])]);
assert.equal(partial.issues.length,0);
assert.deepEqual([...partial.missingParents],['missing']);
assert.equal(partial.closure('a').complete,false);
assert.equal(layoutGraph([...partial.byId.values()]).rows.length,1);

// Adversarial ordering and cross edges: the interval shortcut may prove yes,
// but a failed interval test must never stand in for a negative reachability query.
let seed = 1789;
const next = () => { seed = (Math.imul(seed,1664525)+1013904223)>>>0; return seed; };
for (let run=0;run<6;run++) {
  const nodes=Array.from({length:60},(_,n)=>s(`n${n}`,n?[`n${next()%n}`]:[],n>2?[`n${next()%n}`]:[]));
  if(run%2) nodes.reverse();
  const index = new GraphIndex(nodes,100);
  for (const a of nodes) for(const b of nodes) assert.equal(index.reaches(a.id,b.id),reachesSnapshot(index.byId,a.id,b.id));
  for(const a of nodes) {
    const expected=new Set(nodes.filter(b=>reachesSnapshot(index.byId,a.id,b.id)).map(b=>b.id));
    assert.deepEqual(index.closure(a.id).ids,expected);
    assert.ok(index.stats.cachedMemberships<=100);
  }
}
// Cache lifetime/size and stack depth are independent of provider session length.
const chain=Array.from({length:50_000},(_,n)=>s(String(n),n?[String(n-1)]:[]));
const large=new GraphIndex(chain,1000);
assert.equal(large.issues.length,0);
for(let n=1;n<chain.length;n+=100) assert.equal(large.reaches('49999',String(n)),true);
assert.equal(large.stats.traversed,0,'linear chains use the shared DFS index, not a full walk per query');
assert.equal(large.closure('49999').ids.size,50_000);
assert.equal(large.stats.cachedMemberships,0,'oversized closures are not retained');
for(let n=100;n<1000;n+=100) { large.closure(String(n)); assert.ok(large.stats.cachedMemberships<=1000); }
assert.equal(new GraphIndex([s('a')]).reaches('a','49999'),false,'new repository views do not reuse old query results');
