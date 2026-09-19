import type { GraphState, RepositoryRevision, RepositoryView, Snapshot } from './types';

/** Reject an incomplete or mixed generation rather than locally guessing facts. */
export function validateGraphState(graph: GraphState | undefined, revision?: RepositoryRevision) {
  if (!graph || graph.version !== 1) throw new Error('Unsupported graph contract; refresh after upgrading the server');
  if (!revision || !graph.revision || graph.revision.graph !== revision.graph || graph.revision.pending !== revision.pending ||
    (graph.revision.evidence ?? '0') !== (revision.evidence ?? '0')) throw new Error('Inconsistent graph generation');
  for (const key of ['snapshot_ids','graph_ids','committed_ids','historical_ids','shared_ids','pushed_ids','unpushed_ids','uncommitted_ids','tagged_ids','archived_only_ids','ahead_ids','ahead_tips','markers','hold','orphan_sessions','positions','previous'] as const) {
    if (!Array.isArray(graph[key])) throw new Error(`Incomplete graph contract: ${key}`);
  }
  for (const key of ['branch_snapshots', 'branch_heads', 'ref_scopes', 'snapshot_scopes', 'scope_labels', 'continuations', 'hold_counts'] as const) {
    if (!graph[key] || typeof graph[key] !== 'object' || Array.isArray(graph[key])) throw new Error(`Incomplete graph contract: ${key}`);
  }
  if (!graph.operations || !Array.isArray(graph.operations.births) || !Array.isArray(graph.operations.merges)) throw new Error('Incomplete graph operations');
}

/** Resolve identifiers only. Publication and connectivity were decided upstream. */
export function graphViewRows(view?: RepositoryView) {
  const g = view?.graph;
  const byID = new Map(view?.snapshots.map(s => [s.id,s]));
  const pick = (ids?: string[]) => (ids ?? []).map(id => byID.get(id)).filter((s): s is Snapshot => Boolean(s));
  // Server list order remains newest first; classification sets are deterministic.
  const pickOrdered = (ids?: string[]) => { const keep = new Set(ids); return (view?.snapshots ?? []).filter(s => keep.has(s.id)); };
  const pending = new Map(view?.pending.map(p => [p.session_id,p]));
  const badges = new Map<string,{name:string;kind:string}[]>();
  for (const r of view?.refs ?? []) {
    if ((r.kind !== 'branch' && r.kind !== 'tag') || (r.name.startsWith('cxt/branch-state/') || r.name.startsWith('cxt/history/'))) continue;
    const list=badges.get(r.target) ?? [];list.push({name:r.name,kind:r.kind});badges.set(r.target,list);
  }
  for (const m of g?.markers ?? []) { const list=badges.get(m.target) ?? [];list.push({name:m.branch,kind:m.kind});badges.set(m.target,list); }
  return {
    snapshots:pickOrdered(g?.snapshot_ids),graphSnapshots:pickOrdered(g?.graph_ids),committedSnapshots:pickOrdered(g?.committed_ids),badges,
    uncommittedIds:new Set(g?.uncommitted_ids),sharedIds:new Set(g?.shared_ids),
    localAhead:{ids:new Set(g?.ahead_ids),tips:new Set(g?.ahead_tips)},
    chains:(g?.hold ?? []).map(c=>({tips:c.tips,chain:pick(c.ids)})),
    orphans:(g?.orphan_sessions ?? []).flatMap(id=>pending.has(id)?[pending.get(id)!]:[]),
    holdCount:new Map(Object.entries(g?.hold_counts ?? {})),
  };
}

export function graphStatus(g?: GraphState) {
  const archived=(g?.markers ?? []).filter(m=>m.kind==='archived');
  return {pushed:new Set(g?.pushed_ids),unpushed:new Set(g?.unpushed_ids),uncommitted:new Set(g?.uncommitted_ids),tagged:new Set(g?.tagged_ids),
    archivedOnly:new Set(g?.archived_only_ids),archivedBranches:archived.length,joined:(g?.markers ?? []).filter(m=>m.kind==='joined'),
    archived:archived.map(m=>({...m,uniqueCount:m.unique_count,targetAvailable:m.target_available}))};
}
export function graphProgress(g?: GraphState) {
  return (g?.previous ?? []).map(p=>({...p,createdAt:p.created_at,snapshotIds:new Set(p.snapshot_ids),collapsibleIds:new Set(p.collapsible_ids)}));
}
