import assert from 'node:assert/strict';
import { decodeGraphState, graphIndexFields, type GraphWire } from '../src/graphWire';
import { serverGraphFixture, serverGraphWireFixture } from './serverGraphFixture';
import type { RepositoryView } from '../src/types';
const input: Partial<RepositoryView> = {
  snapshots: ['a','b','c'].map((id,i) => ({id,repo_id:'repo',doc_hash:id,parents:i?[String.fromCharCode(96+i)]:[],branch:'main',provider:'codex',created_at:'2026-09-01T00:00:00Z',message:i===2?'hook: pending':'commit'})),
  refs: [{repo_id:'repo',kind:'branch',name:'main',target:'b'}],
  pending:[{repo_id:'repo',session_id:'session',target:'c',branch:'main',provider:'codex',updated_at:'2026-09-01T00:00:00Z'}],
  unsync:[{repo_id:'repo',branch:'main',target:'c',updated_at:'2026-09-01T00:00:00Z'}],
};
const wire = serverGraphWireFixture(input).graph;
const original = JSON.stringify(wire);
assert.deepEqual(decodeGraphState(wire), serverGraphFixture(input).graph, 'Go transport must preserve the complete domain projection');
assert.equal(JSON.stringify(wire), original, 'decoding must not mutate the cached response');
for (const field of graphIndexFields) {
  for (const invalid of [-1,wire.dictionary.length,0.5,NaN,'a',null]) {
    assert.throws(()=>decodeGraphState({...wire,[field]:[invalid]} as GraphWire),/Invalid graph index/);
  }
}
assert.throws(()=>decodeGraphState({...wire,encoding:'unknown'} as unknown as GraphWire),/Unsupported/);
assert.throws(()=>decodeGraphState({...wire,dictionary:['a','a']}),/Invalid graph dictionary/);
assert.throws(()=>decodeGraphState({...wire,branch_snapshots:{main:[wire.dictionary.length]}}),/Invalid graph index/);
assert.throws(()=>decodeGraphState({...wire,hold:[{tips:[],ids:[-1]}]}),/Invalid graph index/);
assert.throws(()=>decodeGraphState({...wire,previous:[{key:'x',branch:'main',before:'b',after:'a',created_at:'',snapshot_ids:[-1],collapsible_ids:[]}]}),/Invalid graph index/);

const integrationInput: Partial<RepositoryView> = {...input, graph: {...serverGraphFixture(input).graph, branch_contexts: {
  main: {branch_id: 'main', snapshot_id: 'b', reason: 'current', roots: ['b'], snapshot_ids: ['a','b'], merges: []},
}}};
const v2 = serverGraphWireFixture(integrationInput, '', 2).graph;
assert.equal(v2.encoding, 'indexed-v2');
if (v2.encoding !== 'indexed-v2') throw new Error('expected v2');
assert.equal(v2.branch_contexts.main.snapshot_ids_ref, 'main');
assert.equal(v2.branch_contexts.main.snapshot_ids, undefined);
const savedV2 = JSON.stringify(v2);
assert.deepEqual(decodeGraphState(v2), serverGraphFixture(integrationInput, '', 'integrations').graph);
assert.equal(JSON.stringify(v2), savedV2);
for (const invalid of [
  {...v2.branch_contexts.main, roots: [-1]},
  {...v2.branch_contexts.main, snapshot_ids_ref: 'absent'},
  {...v2.branch_contexts.main, snapshot_ids: []},
]) assert.throws(()=>decodeGraphState({...v2, branch_contexts: {main: invalid}}));
const decodedV2 = decodeGraphState(v2);
decodedV2.branch_contexts!.main.snapshot_ids.push('changed');
assert.equal(decodedV2.branch_snapshots.main.includes('changed'), false);
assert.equal(JSON.stringify(v2), savedV2);
