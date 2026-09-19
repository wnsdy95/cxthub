import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import type { RepositoryView, Snapshot, Ref, HistoryEvent, RefLogEntry, ContextSemantics } from '../src/types';
import type { GraphWire } from '../src/graphWire';
import { projectBranchGraph as renderGraph } from '../src/graphProjection';

const state = globalThis as typeof globalThis & { __cxtGraphFixture?: string };
export function serverGraphFixture(input: Partial<RepositoryView>, position = '', mode = ''): RepositoryView {
  if (!state.__cxtGraphFixture) {
    const dir = mkdtempSync(path.join(tmpdir(),'cxt-graph-contract-'));
    state.__cxtGraphFixture = path.join(dir,'project');
    execFileSync('go',['build','-tags=graphfixture','-o',state.__cxtGraphFixture,'./internal/testsupport/graphfixture'],{cwd:path.resolve('..','..','backend')});
    process.once('exit',()=>rmSync(dir,{recursive:true,force:true}));
  }
  const view = {refs:[],snapshots:[],history:[],reflog:[],pending:[],unsync:[],default_branch:'main',revision:{graph:'1',pending:'1'},...input};
  return JSON.parse(execFileSync(state.__cxtGraphFixture,[],{input:JSON.stringify({view,position,mode},(key,value) => value === '' && /(_at|_until)$/.test(key) ? '1970-01-01T00:00:00Z' : value),encoding:'utf8',stdio:['pipe','pipe','pipe'],maxBuffer:64<<20}));
}

export function serverGraphWireFixture(input: Partial<RepositoryView>, position = ''): Omit<RepositoryView, 'graph'> & {graph: GraphWire} {
  // The Go HTTP adapter encodes this response; tests never duplicate the encoder.
  return serverGraphFixture(input, position, 'wire') as unknown as Omit<RepositoryView, 'graph'> & {graph: GraphWire};
}

// Existing layout regressions now exercise Go's business plan plus TS rendering.
export function projectBranchGraph(snapshots: Snapshot[],refs: Ref[],history: HistoryEvent[],reflog: RefLogEntry[],pinHead?: string|null,pinBranch?: string,_index?: unknown,_evidence?: unknown) {
  const view = serverGraphFixture({snapshots,refs,history,reflog,default_branch:pinBranch ?? 'main'});
  return renderGraph(snapshots,refs,view.graph,pinHead,pinBranch);
}
export function serverSemantics(snapshots:Snapshot[],history:HistoryEvent[]): ContextSemantics {
  return serverGraphFixture({snapshots,history},'','semantics').semantics!;
}

export { hiddenProgressIds } from '../src/contextHistory';
import { graphStatus, graphProgress, graphViewRows } from '../src/graphState';
import { completedBranchEvidence as consumeEvidence } from '../src/graphEvidence';
import type { Pending, Unsync } from '../src/types';
const retainedRefs = (ids: Iterable<string>): Ref[] => [...ids].map(target=>({repo_id:'repo',kind:'session',name:'retained:'+target,target}));
export function completedBranchEvidence(s:Snapshot[],h:HistoryEvent[],_index?:unknown,semantics?:ContextSemantics) {
 return consumeEvidence(s,h,undefined,semantics ?? serverSemantics(s,h));
}
export function historicalSnapshotIds(reflog:RefLogEntry[],snapshots:Snapshot[],history:HistoryEvent[]=[]) { return new Set(serverGraphFixture({snapshots,reflog,history}).graph.historical_ids); }
export function sharedReachable(refs:Ref[],snapshots:Snapshot[]) { return new Set(serverGraphFixture({snapshots,refs}).graph.shared_ids); }
export function reachableSnapshotIds(ids:Iterable<string>,snapshots:Snapshot[]) {return sharedReachable(retainedRefs(ids),snapshots);}
export function classifyBranchHistoryMarkers(refs:Ref[],snapshots:Snapshot[],primary='main',history:HistoryEvent[]=[]) {
 return serverGraphFixture({refs,snapshots,history,default_branch:primary}).graph.markers.map(({branch,target,kind})=>({branch,target,kind}));
}
export function classifyGraphSnapshots(refs:Ref[],snapshots:Snapshot[],pendingIDs:ReadonlySet<string>=new Set(),primary='main',historical:ReadonlySet<string>=new Set(),history:HistoryEvent[]=[]) {
 const pending=[...pendingIDs].map(target=>({repo_id:'repo',session_id:target,branch:'main',provider:'codex',target,updated_at:'1970-01-01T00:00:00Z'}));
 const out=graphStatus(serverGraphFixture({refs:[...refs,...retainedRefs(historical)],snapshots,pending,history,default_branch:primary}).graph);
 return {...out,joined:out.joined.map(({branch,target,kind})=>({branch,target,kind})),archived:out.archived.map(({branch,target,uniqueCount,targetAvailable})=>({branch,target,uniqueCount,targetAvailable}))};
}
export function previousProgressGroups(refs:Ref[],snapshots:Snapshot[],reflog:RefLogEntry[],history:HistoryEvent[]=[],position?:{branch:string;branch_id?:string;snapshot:string}) {
 const event:HistoryEvent|undefined=position?{id:'fixture-position',repo_id:'repo',kind:'position',branch:position.branch,branch_id:position.branch_id ?? '',target:position.snapshot,created_at:'1970-01-01T00:00:00Z'}:undefined;
 return graphProgress(serverGraphFixture({refs,snapshots,reflog,history:event?[...history,event]:history},event?.id).graph);
}
export function unsyncChains(unsync:Unsync[],snapshots:Snapshot[],shared:Set<string>) {return graphViewRows(serverGraphFixture({refs:retainedRefs(shared),snapshots,unsync})).chains;}
export function orphanPendings(pending:Pending[],refs:Ref[],snapshots:Snapshot[],clusters:{tips:Unsync[]}[],shared=sharedReachable(refs,snapshots)) {return graphViewRows(serverGraphFixture({refs:[...refs,...retainedRefs(shared)],snapshots,pending,unsync:clusters.flatMap(c=>c.tips)})).orphans;}
export function holdCounts(refs:Ref[],snapshots:Snapshot[],unsync:Unsync[],pending:Pending[],shared=sharedReachable(refs,snapshots)) {return graphViewRows(serverGraphFixture({refs:[...refs,...retainedRefs(shared)],snapshots,pending,unsync})).holdCount;}
export function repositoryGraph(snapshots:Snapshot[],refs:Ref[],history:HistoryEvent[],shared:Set<string>,pending:Pending[],unsync:Unsync[]) {const v=serverGraphFixture({refs:[...refs,...retainedRefs(shared)],snapshots,history,pending,unsync});return {...graphViewRows(v),graphState:v.graph};}
